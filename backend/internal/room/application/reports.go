package application

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

var ErrInvalidReport = errors.New("invalid problem report")

type ProblemReport struct {
	AccountID   string
	Description string
	Section     string
	AppVersion  string
}

type ReportStore interface {
	SaveProblemReport(context.Context, ProblemReport) error
}

func (s *Service) CreateProblemReport(ctx context.Context, report ProblemReport) error {
	report.Description = strings.TrimSpace(report.Description)
	report.AppVersion = strings.TrimSpace(report.AppVersion)
	if report.AccountID == "" || report.Description == "" || utf8.RuneCountInString(report.Description) > 5000 ||
		report.AppVersion == "" || utf8.RuneCountInString(report.AppVersion) > 64 ||
		strings.ContainsRune(report.Description, 0) || strings.ContainsRune(report.AppVersion, 0) {
		return ErrInvalidReport
	}
	switch report.Section {
	case "main_menu", "create_room", "join_room", "wallet", "waiting_room", "gameplay", "round_result", "next_round", "connection", "account":
	default:
		return ErrInvalidReport
	}
	store, ok := s.repository.(ReportStore)
	if !ok {
		return errors.New("repository does not support problem reports")
	}
	return store.SaveProblemReport(ctx, report)
}
