import { NgModule } from '@angular/core';
import { CommonModule } from '@angular/common';
import { AppPortalComponent } from './app-portal.component';
import { FormsModule, ReactiveFormsModule } from '@angular/forms';
import { MatDatepickerModule } from '@angular/material/datepicker';
import { MatNativeDateModule } from '@angular/material/core';
import { Routes, RouterModule } from '@angular/router';

const routes: Routes = [{ path: '', component: AppPortalComponent }];

@NgModule({
	declarations: [AppPortalComponent],
	imports: [CommonModule, RouterModule.forChild(routes), FormsModule, ReactiveFormsModule, MatDatepickerModule, MatNativeDateModule],
	providers: [MatDatepickerModule],
	exports: [AppPortalComponent]
})
export class AppPortalModule {}
