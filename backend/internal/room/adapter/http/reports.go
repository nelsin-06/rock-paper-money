package roomhttp

import (
	"errors"
	"net/http"

	"example.com/rock-paper-money/internal/auth"
	"example.com/rock-paper-money/internal/room/application"
)

func (rt *router) createProblemReport(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	var body struct {
		Description string `json:"description"`
		Section     string `json:"section"`
		AppVersion  string `json:"app_version"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if !decodeJSONBody(r, &body) {
		writeAPIError(w, r, errorInvalidBody)
		return
	}
	err := rt.service.CreateProblemReport(r.Context(), application.ProblemReport{
		AccountID: principal.Subject, Description: body.Description, Section: body.Section, AppVersion: body.AppVersion,
	})
	if errors.Is(err, application.ErrInvalidReport) {
		writeAPIError(w, r, publicError(http.StatusBadRequest, "invalid_problem_report", "Choose a valid section and provide a description (1–5000 characters) and app version (1–64 characters)."))
		return
	}
	if err != nil {
		writeInternalError(w, r, "create_problem_report", err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Status string `json:"status"`
	}{Status: "created"})
}
