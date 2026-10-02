//go:build unit

package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"github.com/MACOS-DO/sub4api/internal/service"
)

// Including soft-deleted rows must not turn a genuinely missing row or a
// database failure into a successful, partially applied billing transaction.
func TestUsageBillingRepositoryApply_KeyUpdateFailureStillRollsBack(t *testing.T) {
	dbFailure := errors.New("database unavailable")
	for _, field := range []string{"quota", "window"} {
		for _, missing := range []bool{false, true} {
			name := field + "/database_error"
			if missing {
				name = field + "/missing_row"
			}
			t.Run(name, func(t *testing.T) {
				db, mock, err := sqlmock.New()
				require.NoError(t, err)
				defer func() { _ = db.Close() }()
				cmd := &service.UsageBillingCommand{
					RequestID: "failed-key-settlement", APIKeyID: 7, UserID: 42, BalanceCost: 1.25,
				}
				mock.ExpectBegin()
				mock.ExpectQuery("INSERT INTO usage_billing_dedup").
					WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))
				mock.ExpectQuery("SELECT request_fingerprint.*FROM usage_billing_dedup_archive").
					WillReturnRows(sqlmock.NewRows([]string{"request_fingerprint"}))
				mock.ExpectQuery(conditionalBalanceDeductSQL).
					WithArgs(1.25, int64(42)).
					WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(98.75))
				if field == "quota" {
					cmd.APIKeyQuotaCost = 1.25
					q := mock.ExpectQuery("UPDATE api_keys").
						WithArgs(1.25, int64(7), service.StatusAPIKeyActive, service.StatusAPIKeyQuotaExhausted)
					if missing {
						q.WillReturnError(sql.ErrNoRows)
					} else {
						q.WillReturnError(dbFailure)
					}
				} else {
					cmd.APIKeyRateLimitCost = 1.25
					q := mock.ExpectExec("UPDATE api_keys").WithArgs(1.25, int64(7))
					if missing {
						q.WillReturnResult(sqlmock.NewResult(0, 0))
					} else {
						q.WillReturnError(dbFailure)
					}
				}
				mock.ExpectRollback()
				result, err := NewUsageBillingRepository(nil, db).Apply(context.Background(), cmd)
				require.Nil(t, result)
				wantErr := dbFailure
				if missing {
					wantErr = service.ErrAPIKeyNotFound
				}
				require.ErrorIs(t, err, wantErr)
				require.NoError(t, mock.ExpectationsWereMet())
			})
		}
	}
}
