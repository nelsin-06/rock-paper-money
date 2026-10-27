package postgres

import (
	"context"

	"example.com/rock-paper-money/internal/room/application"
)

func (r *Repository) SaveProblemReport(ctx context.Context, report application.ProblemReport) error {
	_, err := r.pool.Exec(ctx,
		"INSERT INTO problem_reports(account_id, description, section, app_version) VALUES ($1, $2, $3, $4)",
		report.AccountID, report.Description, report.Section, report.AppVersion)
	return err
}
