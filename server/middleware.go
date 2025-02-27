package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/frain-dev/convoy/logger"
	"github.com/frain-dev/convoy/tracer"
	"github.com/newrelic/go-agent/v3/newrelic"

	"github.com/frain-dev/convoy/auth"

	"go.mongodb.org/mongo-driver/mongo"

	"github.com/felixge/httpsnoop"
	"github.com/frain-dev/convoy/auth/realm_chain"
	"github.com/frain-dev/convoy/config"
	"github.com/frain-dev/convoy/datastore"
	"github.com/frain-dev/convoy/server/models"
	"github.com/frain-dev/convoy/util"

	log "github.com/sirupsen/logrus"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/render"
)

type contextKey string

const (
	groupCtx         contextKey = "group"
	appCtx           contextKey = "app"
	endpointCtx      contextKey = "endpoint"
	eventCtx         contextKey = "event"
	eventDeliveryCtx contextKey = "eventDelivery"
	configCtx        contextKey = "configCtx"
	//authConfigCtx       contextKey = "authConfig"
	authLoginCtx        contextKey = "authLogin"
	authUserCtx         contextKey = "authUser"
	pageableCtx         contextKey = "pageable"
	pageDataCtx         contextKey = "pageData"
	dashboardCtx        contextKey = "dashboard"
	deliveryAttemptsCtx contextKey = "deliveryAttempts"
	baseUrlCtx          contextKey = "baseUrl"
	appIdCtx            contextKey = "appId"
	groupIdCtx          contextKey = "groupId"
)

func instrumentPath(path string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m := httpsnoop.CaptureMetrics(next, w, r)
			requestDuration.WithLabelValues(r.Method, path,
				strconv.Itoa(m.Code)).Observe(m.Duration.Seconds())
		})
	}
}

func instrumentRequests(tr tracer.Tracer) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cfg, err := config.Get()

			if err != nil {
				log.WithError(err).Error("failed to load configuration")
				return
			}

			if cfg.Tracer.Type == config.NewRelicTracerProvider {
				txn := tr.StartTransaction(r.URL.Path)
				defer txn.End()

				tr.SetWebRequestHTTP(r, txn)
				w = tr.SetWebResponse(w, txn)
				r = tr.RequestWithTransactionContext(r, txn)
			}

			next.ServeHTTP(w, r)
		})
	}
}

func writeRequestIDHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", r.Context().Value(middleware.RequestIDKey).(string))
		next.ServeHTTP(w, r)
	})
}

func setupCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg, err := config.Get()
		if err != nil {
			log.WithError(err).Error("failed to load configuration")
			return
		}

		if env := cfg.Environment; string(env) == "development" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")
		}

		if r.Method == "OPTIONS" {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func jsonResponse(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

func requireApp(appRepo datastore.ApplicationRepository) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			appID := chi.URLParam(r, "appID")

			app, err := appRepo.FindApplicationByID(r.Context(), appID)
			if err != nil {

				event := "an error occurred while retrieving app details"
				statusCode := http.StatusBadRequest

				if errors.Is(err, datastore.ErrApplicationNotFound) {
					event = err.Error()
					statusCode = http.StatusNotFound
				}

				_ = render.Render(w, r, newErrorResponse(event, statusCode))
				return
			}

			r = r.WithContext(setApplicationInContext(r.Context(), app))
			next.ServeHTTP(w, r)
		})
	}
}

func requireAppID() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authUser := getAuthUserFromContext(r.Context())

			if len(authUser.Role.Apps) > 0 {
				appID := authUser.Role.Apps[0]
				r = r.WithContext(setAppIDInContext(r.Context(), appID))
			}

			next.ServeHTTP(w, r)
		})
	}
}

func requireAppPortalApplication(appRepo datastore.ApplicationRepository) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			appID := chi.URLParam(r, "appID")

			if util.IsStringEmpty(appID) {
				appID = r.URL.Query().Get("appId")
			}

			if util.IsStringEmpty(appID) {
				appID = getAppIDFromContext(r.Context())
			}

			app, err := appRepo.FindApplicationByID(r.Context(), appID)
			if err != nil {

				event := "an error occurred while retrieving app details"
				statusCode := http.StatusBadRequest

				if errors.Is(err, datastore.ErrApplicationNotFound) {
					event = err.Error()
					statusCode = http.StatusBadRequest
				}

				_ = render.Render(w, r, newErrorResponse(event, statusCode))
				return
			}

			r = r.WithContext(setApplicationInContext(r.Context(), app))
			next.ServeHTTP(w, r)
		})
	}
}

func requireAppPortalPermission(role auth.RoleType) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authUser := getAuthUserFromContext(r.Context())
			if authUser.Role.Type.Is(auth.RoleSuperUser) {
				// superuser has access to everything
				next.ServeHTTP(w, r)
				return
			}

			if !authUser.Role.Type.Is(role) {
				_ = render.Render(w, r, newErrorResponse("unauthorized role", http.StatusUnauthorized))
				return
			}

			group := getGroupFromContext(r.Context())
			for _, v := range authUser.Role.Groups {
				if group.Name == v || group.UID == v {

					if len(authUser.Role.Apps) > 0 { //we're dealing with an app portal token at this point
						app := getApplicationFromContext(r.Context())

						for _, ap := range authUser.Role.Apps {
							if app.Title == ap || app.UID == ap {
								next.ServeHTTP(w, r)
								return
							}
						}

						_ = render.Render(w, r, newErrorResponse("unauthorized access", http.StatusUnauthorized))
						return
					}

					next.ServeHTTP(w, r)
					return
				}
			}

			_ = render.Render(w, r, newErrorResponse("unauthorized to access group", http.StatusUnauthorized))
		})
	}
}

func filterDeletedEndpoints(endpoints []datastore.Endpoint) []datastore.Endpoint {
	activeEndpoints := make([]datastore.Endpoint, 0)
	for _, endpoint := range endpoints {
		if endpoint.DeletedAt == 0 {
			activeEndpoints = append(activeEndpoints, endpoint)
		}
	}
	return activeEndpoints
}

func parseEndpointFromBody(r *http.Request) (models.Endpoint, error) {
	var e models.Endpoint
	err := util.ReadJSON(r, &e)
	if err != nil {
		return e, err
	}

	description := e.Description
	if util.IsStringEmpty(description) {
		return e, errors.New("please provide a description")
	}

	e.URL, err = util.CleanEndpoint(e.URL)
	if err != nil {
		return e, err
	}

	return e, nil
}

func requireAppEndpoint() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			app := getApplicationFromContext(r.Context())
			endPointId := chi.URLParam(r, "endpointID")

			endpoint, err := findEndpoint(&app.Endpoints, endPointId)
			if err != nil {
				_ = render.Render(w, r, newErrorResponse(err.Error(), http.StatusBadRequest))
				return
			}

			r = r.WithContext(setApplicationEndpointInContext(r.Context(), endpoint))
			next.ServeHTTP(w, r)
		})
	}
}

func requireEvent(eventRepo datastore.EventRepository) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			eventId := chi.URLParam(r, "eventID")

			event, err := eventRepo.FindEventByID(r.Context(), eventId)
			if err != nil {

				event := "an error occurred while retrieving event details"
				statusCode := http.StatusInternalServerError

				if errors.Is(err, datastore.ErrEventNotFound) {
					event = err.Error()
					statusCode = http.StatusNotFound
				}

				_ = render.Render(w, r, newErrorResponse(event, statusCode))
				return
			}

			r = r.WithContext(setEventInContext(r.Context(), event))
			next.ServeHTTP(w, r)
		})
	}
}

func requireEventDelivery(eventRepo datastore.EventDeliveryRepository) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			eventDeliveryID := chi.URLParam(r, "eventDeliveryID")

			eventDelivery, err := eventRepo.FindEventDeliveryByID(r.Context(), eventDeliveryID)
			if err != nil {

				eventDelivery := "an error occurred while retrieving event delivery details"
				statusCode := http.StatusInternalServerError

				if errors.Is(err, datastore.ErrEventDeliveryNotFound) {
					eventDelivery = err.Error()
					statusCode = http.StatusNotFound
				}

				_ = render.Render(w, r, newErrorResponse(eventDelivery, statusCode))
				return
			}

			r = r.WithContext(setEventDeliveryInContext(r.Context(), eventDelivery))
			next.ServeHTTP(w, r)
		})
	}
}

func requireDeliveryAttempt() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			id := chi.URLParam(r, "deliveryAttemptID")

			attempts := getDeliveryAttemptsFromContext(r.Context())

			attempt, err := findMessageDeliveryAttempt(attempts, id)
			if err != nil {
				_ = render.Render(w, r, newErrorResponse(err.Error(), http.StatusBadRequest))
				return
			}

			r = r.WithContext(setDeliveryAttemptInContext(r.Context(), attempt))
			next.ServeHTTP(w, r)
		})
	}
}

func findEndpoint(endpoints *[]datastore.Endpoint, id string) (*datastore.Endpoint, error) {
	for _, endpoint := range *endpoints {
		if endpoint.UID == id && endpoint.DeletedAt == 0 {
			return &endpoint, nil
		}
	}
	return nil, datastore.ErrEndpointNotFound
}

func getDefaultGroup(r *http.Request, groupRepo datastore.GroupRepository) (*datastore.Group, error) {

	groups, err := groupRepo.LoadGroups(r.Context(), &datastore.GroupFilter{Names: []string{"default-group"}})
	if err != nil {
		return nil, err
	}

	if !(len(groups) > 0) {
		return nil, errors.New("no default group, please your config")
	}

	return groups[0], err
}

func requireGroupID() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authUser := getAuthUserFromContext(r.Context())

			if len(authUser.Role.Groups) > 0 && authUser.Credential.Type == auth.CredentialTypeAPIKey {
				groupID := authUser.Role.Groups[0]
				r = r.WithContext(setGroupIDInContext(r.Context(), groupID))
			}

			next.ServeHTTP(w, r)
		})
	}
}

func requireGroup(groupRepo datastore.GroupRepository) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var group *datastore.Group
			var err error
			var groupID string

			groupID = r.URL.Query().Get("groupID")

			if util.IsStringEmpty(groupID) {
				groupID = chi.URLParam(r, "groupID")
			}

			if util.IsStringEmpty(groupID) {
				groupID = getGroupIDFromContext(r.Context())
			}

			if !util.IsStringEmpty(groupID) {
				group, err = groupRepo.FetchGroupByID(r.Context(), groupID)
				if err != nil {
					_ = render.Render(w, r, newErrorResponse("failed to fetch group by id", http.StatusInternalServerError))
					return
				}
			} else {
				group, err = getDefaultGroup(r, groupRepo)
				if err != nil {
					event := "an error occurred while loading default group"
					statusCode := http.StatusInternalServerError

					// TODO(daniel,subomi): this should be impossible, because we call ensureDefaultGroup on app startup, find a better way to report this?
					if errors.Is(err, mongo.ErrNoDocuments) {
						event = err.Error()
						statusCode = http.StatusNotFound
					}

					_ = render.Render(w, r, newErrorResponse(event, statusCode))
					return
				}
			}
			r = r.WithContext(setGroupInContext(r.Context(), group))
			next.ServeHTTP(w, r)
		})
	}
}

func requireAuth() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			creds, err := getAuthFromRequest(r)
			if err != nil {
				log.WithError(err).Error("failed to get auth from request")
				_ = render.Render(w, r, newErrorResponse(err.Error(), http.StatusUnauthorized))
				return
			}

			rc, err := realm_chain.Get()
			if err != nil {
				log.WithError(err).Error("failed to get realm chain")
				_ = render.Render(w, r, newErrorResponse("internal server error", http.StatusInternalServerError))
				return
			}

			authUser, err := rc.Authenticate(r.Context(), creds)
			if err != nil {
				log.WithError(err).Error("failed to authenticate")
				_ = render.Render(w, r, newErrorResponse("authorization failed", http.StatusUnauthorized))
				return
			}

			r = r.WithContext(setAuthUserInContext(r.Context(), authUser))
			next.ServeHTTP(w, r)
		})
	}
}

func requireBaseUrl() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cfg, err := config.Get()
			if err != nil {
				log.WithError(err).Error("failed to load configuration")
				return
			}

			r = r.WithContext(setBaseUrlInContext(r.Context(), cfg.BaseUrl))
			next.ServeHTTP(w, r)
		})
	}
}

func requirePermission(role auth.RoleType) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authUser := getAuthUserFromContext(r.Context())
			if authUser.Role.Type.Is(auth.RoleSuperUser) {
				// superuser has access to everything
				next.ServeHTTP(w, r)
				return
			}

			if !authUser.Role.Type.Is(role) {
				_ = render.Render(w, r, newErrorResponse("unauthorized role", http.StatusUnauthorized))
				return
			}

			group := getGroupFromContext(r.Context())
			for _, v := range authUser.Role.Groups {
				if group.Name == v || group.UID == v {

					if len(authUser.Role.Apps) > 0 { //we're dealing with an app portal token at this point
						_ = render.Render(w, r, newErrorResponse("unauthorized to access group", http.StatusUnauthorized))
						return
					}

					next.ServeHTTP(w, r)
					return
				}
			}

			_ = render.Render(w, r, newErrorResponse("unauthorized to access group", http.StatusUnauthorized))
		})
	}
}

func getAuthFromRequest(r *http.Request) (*auth.Credential, error) {
	cfg, err := config.Get()
	if err != nil {
		log.WithError(err)
		return nil, err
	}

	if !cfg.Auth.RequireAuth {
		return nil, nil
	}

	val := r.Header.Get("Authorization")
	authInfo := strings.Split(val, " ")

	if len(authInfo) != 2 {
		return nil, errors.New("invalid header structure")
	}

	credType := auth.CredentialType(strings.ToUpper(authInfo[0]))
	switch credType {
	case auth.CredentialTypeBasic:

		credentials, err := base64.StdEncoding.DecodeString(authInfo[1])
		if err != nil {
			return nil, errors.New("invalid credentials")
		}

		creds := strings.Split(string(credentials), ":")

		if len(creds) != 2 {
			return nil, errors.New("invalid basic credentials")
		}

		return &auth.Credential{
			Type:     auth.CredentialTypeBasic,
			Username: creds[0],
			Password: creds[1],
		}, nil
	case auth.CredentialTypeAPIKey:
		if util.IsStringEmpty(authInfo[1]) {
			return nil, errors.New("empty api key")
		}

		return &auth.Credential{
			Type:   auth.CredentialTypeAPIKey,
			APIKey: authInfo[1],
		}, nil
	default:
		return nil, fmt.Errorf("unknown credential type: %s", credType.String())
	}
}

func pagination(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawPerPage := r.URL.Query().Get("perPage")
		rawPage := r.URL.Query().Get("page")
		rawSort := r.URL.Query().Get("sort")

		if len(rawPerPage) == 0 {
			rawPerPage = "20"
		}
		if len(rawPage) == 0 {
			rawPage = "0"
		}
		if len(rawSort) == 0 {
			rawSort = "-1"
		}

		var err error
		var sort = -1 // desc by default
		order := strings.ToLower(rawSort)
		if order == "asc" {
			sort = 1
		}

		var perPage int
		if perPage, err = strconv.Atoi(rawPerPage); err != nil {
			perPage = 20
		}

		var page int
		if page, err = strconv.Atoi(rawPage); err != nil {
			page = 0
		}
		pageable := datastore.Pageable{
			Page:    page,
			PerPage: perPage,
			Sort:    sort,
		}
		r = r.WithContext(setPageableInContext(r.Context(), pageable))
		next.ServeHTTP(w, r)
	})
}

func logHttpRequest(log logger.Logger) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			if logger.CanLogHttpRequest(log) {
				start := time.Now()

				defer func() {
					requestFields := requestLogFields(r)
					responseFields := responseLogFields(ww, start)

					logFields := map[string]interface{}{
						"httpRequest":  requestFields,
						"httpResponse": responseFields,
					}

					log.WithLogger().WithFields(logFields).Log(statusLevel(ww.Status()), requestFields["requestURL"])
				}()

			}
			next.ServeHTTP(ww, r)
		})
	}
}

func requestLogFields(r *http.Request) map[string]interface{} {
	scheme := "http"

	if r.TLS != nil {
		scheme = "https"
	}

	requestURL := fmt.Sprintf("%s://%s%s", scheme, r.Host, r.RequestURI)

	requestFields := map[string]interface{}{
		"requestURL":    requestURL,
		"requestMethod": r.Method,
		"requestPath":   r.URL.Path,
		"remoteIP":      r.RemoteAddr,
		"proto":         r.Proto,
		"scheme":        scheme,
	}

	if reqID := middleware.GetReqID(r.Context()); reqID != "" {
		requestFields["x-request-id"] = reqID
	}

	if len(r.Header) > 0 {
		requestFields["header"] = headerFields(r.Header)
	}

	cfg, err := config.Get()
	if err != nil {
		log.WithError(err).Error("failed to load configuration")
		return nil
	}

	if cfg.Tracer.Type == config.NewRelicTracerProvider {
		txn := newrelic.FromContext(r.Context()).GetLinkingMetadata()

		if cfg.NewRelic.DistributedTracerEnabled {
			requestFields["traceID"] = txn.TraceID
			requestFields["spanID"] = txn.SpanID
		}

		requestFields["entityGUID"] = txn.EntityGUID
		requestFields["entityType"] = txn.EntityType
	}

	return requestFields
}

func responseLogFields(w middleware.WrapResponseWriter, t time.Time) map[string]interface{} {
	responseFields := map[string]interface{}{
		"status":  w.Status(),
		"byes":    w.BytesWritten(),
		"latency": time.Since(t),
	}

	if len(w.Header()) > 0 {
		responseFields["header"] = headerFields(w.Header())
	}

	return responseFields
}

func statusLevel(status int) log.Level {
	switch {
	case status <= 0:
		return log.WarnLevel
	case status < 400:
		return log.InfoLevel
	case status >= 400 && status < 500:
		return log.WarnLevel
	case status >= 500:
		return log.ErrorLevel
	default:
		return log.InfoLevel
	}
}

func headerFields(header http.Header) map[string]string {
	headerField := map[string]string{}

	for k, v := range header {
		k = strings.ToLower(k)
		switch {
		case len(v) == 0:
			continue
		case len(v) == 1:
			headerField[k] = v[0]
		default:
			headerField[k] = fmt.Sprintf("[%s]", strings.Join(v, "], ["))
		}
		if k == "authorization" || k == "cookie" || k == "set-cookie" {
			headerField[k] = "***"
		}
	}

	return headerField
}

func fetchGroupApps(appRepo datastore.ApplicationRepository) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			pageable := getPageableFromContext(r.Context())

			group := getGroupFromContext(r.Context())

			apps, paginationData, err := appRepo.LoadApplicationsPagedByGroupId(r.Context(), group.UID, pageable)
			if err != nil {
				_ = render.Render(w, r, newErrorResponse("an error occurred while fetching apps", http.StatusInternalServerError))
				return
			}

			r = r.WithContext(setApplicationsInContext(r.Context(), &apps))
			r = r.WithContext(setPaginationDataInContext(r.Context(), &paginationData))
			next.ServeHTTP(w, r)
		})
	}
}

func ensurePeriod(start time.Time, end time.Time) error {
	if start.Unix() > end.Unix() {
		return errors.New("startDate cannot be greater than endDate")
	}

	return nil
}

func computeDashboardMessages(ctx context.Context, orgId string, eventRepo datastore.EventRepository, searchParams datastore.SearchParams, period datastore.Period) (uint64, []datastore.EventInterval, error) {

	var messagesSent uint64

	messages, err := eventRepo.LoadEventIntervals(ctx, orgId, searchParams, period, 1)
	if err != nil {
		log.Errorln("failed to load message intervals - ", err)
		return 0, nil, err
	}

	for _, m := range messages {
		messagesSent += m.Count
	}

	return messagesSent, messages, nil
}

func setApplicationInContext(ctx context.Context,
	app *datastore.Application) context.Context {
	return context.WithValue(ctx, appCtx, app)
}

func getApplicationFromContext(ctx context.Context) *datastore.Application {
	return ctx.Value(appCtx).(*datastore.Application)
}

func setEventInContext(ctx context.Context,
	event *datastore.Event) context.Context {
	return context.WithValue(ctx, eventCtx, event)
}

func getEventFromContext(ctx context.Context) *datastore.Event {
	return ctx.Value(eventCtx).(*datastore.Event)
}

func setEventDeliveryInContext(ctx context.Context,
	eventDelivery *datastore.EventDelivery) context.Context {
	return context.WithValue(ctx, eventDeliveryCtx, eventDelivery)
}

func getEventDeliveryFromContext(ctx context.Context) *datastore.EventDelivery {
	return ctx.Value(eventDeliveryCtx).(*datastore.EventDelivery)
}

func setApplicationsInContext(ctx context.Context,
	apps *[]datastore.Application) context.Context {
	return context.WithValue(ctx, appCtx, apps)
}

func getApplicationsFromContext(ctx context.Context) *[]datastore.Application {
	return ctx.Value(appCtx).(*[]datastore.Application)
}

func setApplicationEndpointInContext(ctx context.Context,
	endpoint *datastore.Endpoint) context.Context {
	return context.WithValue(ctx, endpointCtx, endpoint)
}

func getApplicationEndpointFromContext(ctx context.Context) *datastore.Endpoint {
	return ctx.Value(endpointCtx).(*datastore.Endpoint)
}

func setGroupInContext(ctx context.Context, group *datastore.Group) context.Context {
	return context.WithValue(ctx, groupCtx, group)
}

func getGroupFromContext(ctx context.Context) *datastore.Group {
	return ctx.Value(groupCtx).(*datastore.Group)
}

func setPageableInContext(ctx context.Context, pageable datastore.Pageable) context.Context {
	return context.WithValue(ctx, pageableCtx, pageable)
}

func getPageableFromContext(ctx context.Context) datastore.Pageable {
	return ctx.Value(pageableCtx).(datastore.Pageable)
}

func setPaginationDataInContext(ctx context.Context, p *datastore.PaginationData) context.Context {
	return context.WithValue(ctx, pageDataCtx, p)
}

func getPaginationDataFromContext(ctx context.Context) *datastore.PaginationData {
	return ctx.Value(pageDataCtx).(*datastore.PaginationData)
}

func setDashboardSummaryInContext(ctx context.Context, d *models.DashboardSummary) context.Context {
	return context.WithValue(ctx, dashboardCtx, d)
}

func getDashboardSummaryFromContext(ctx context.Context) *models.DashboardSummary {
	return ctx.Value(dashboardCtx).(*models.DashboardSummary)
}

func setDeliveryAttemptInContext(ctx context.Context,
	attempt *datastore.DeliveryAttempt) context.Context {
	return context.WithValue(ctx, deliveryAttemptsCtx, attempt)
}

func getDeliveryAttemptFromContext(ctx context.Context) *datastore.DeliveryAttempt {
	return ctx.Value(deliveryAttemptsCtx).(*datastore.DeliveryAttempt)
}

func setDeliveryAttemptsInContext(ctx context.Context,
	attempts *[]datastore.DeliveryAttempt) context.Context {
	return context.WithValue(ctx, deliveryAttemptsCtx, attempts)
}

func getDeliveryAttemptsFromContext(ctx context.Context) *[]datastore.DeliveryAttempt {
	return ctx.Value(deliveryAttemptsCtx).(*[]datastore.DeliveryAttempt)
}

func setAuthUserInContext(ctx context.Context, a *auth.AuthenticatedUser) context.Context {
	return context.WithValue(ctx, authUserCtx, a)
}

func getAuthUserFromContext(ctx context.Context) *auth.AuthenticatedUser {
	return ctx.Value(authUserCtx).(*auth.AuthenticatedUser)
}

func getAuthLoginFromContext(ctx context.Context) *AuthorizedLogin {
	return ctx.Value(authLoginCtx).(*AuthorizedLogin)
}

func setConfigInContext(ctx context.Context, c *ViewableConfiguration) context.Context {
	return context.WithValue(ctx, configCtx, c)
}

func getConfigFromContext(ctx context.Context) *ViewableConfiguration {
	return ctx.Value(configCtx).(*ViewableConfiguration)
}

func setBaseUrlInContext(ctx context.Context, baseUrl string) context.Context {
	return context.WithValue(ctx, baseUrlCtx, baseUrl)
}

func getBaseUrlFromContext(ctx context.Context) string {
	return ctx.Value(baseUrlCtx).(string)
}

func setAppIDInContext(ctx context.Context, appId string) context.Context {
	return context.WithValue(ctx, appIdCtx, appId)
}

func getAppIDFromContext(ctx context.Context) string {
	var appID string

	if appID, ok := ctx.Value(appIdCtx).(string); ok {
		return appID
	}

	return appID
}

func setGroupIDInContext(ctx context.Context, groupId string) context.Context {
	return context.WithValue(ctx, groupIdCtx, groupId)
}

func getGroupIDFromContext(ctx context.Context) string {
	var groupID string

	if groupID, ok := ctx.Value(groupIdCtx).(string); ok {
		return groupID
	}

	return groupID
}
