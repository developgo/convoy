package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/frain-dev/convoy/config"
	"github.com/frain-dev/convoy/datastore"
	"github.com/frain-dev/convoy/mocks"
	"github.com/golang/mock/gomock"
	log "github.com/sirupsen/logrus"
)

func Test_fetchAllConfigDetails(t *testing.T) {
	var app *applicationHandler

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	app = provideApplication(ctrl)

	tests := []struct {
		name       string
		method     string
		statusCode int
		dbFn       func(app *applicationHandler)
	}{
		{
			name:       "successful config fetch",
			method:     http.MethodGet,
			statusCode: http.StatusOK,
			dbFn: func(app *applicationHandler) {
				g, _ := app.groupRepo.(*mocks.MockGroupRepository)

				g.EXPECT().
					FetchGroupByID(gomock.Any(), gomock.Any()).Times(1).
					Return(&datastore.Group{
						Config: &datastore.GroupConfig{},
					}, nil)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := config.LoadConfig("./testdata/Auth_Config/none-convoy.json")
			if err != nil {
				t.Errorf("Failed to load config file: %v", err)
			}
			initRealmChain(t, app.apiKeyRepo)

			if tc.dbFn != nil {
				tc.dbFn(app)
			}

			req := httptest.NewRequest(tc.method, "/ui/dashboard/config?groupID=12345", nil)
			responseRecorder := httptest.NewRecorder()

			requireGroup(app.groupRepo)(http.HandlerFunc(app.GetAllConfigDetails)).
				ServeHTTP(responseRecorder, req)

			if responseRecorder.Code != tc.statusCode {
				log.Error(tc.name, responseRecorder.Body)
				t.Errorf("Want status '%d', got '%d'", tc.statusCode, responseRecorder.Code)
			}

		})
	}
}
