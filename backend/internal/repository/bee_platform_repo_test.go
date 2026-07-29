package repository

import (
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestTranslateBeePlatformPersistenceError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantErr    error
		constraint string
	}{
		{
			name:       "same bee platform",
			err:        &pq.Error{Code: "23505", Constraint: "uq_bee_platform_active_bee_platform"},
			wantErr:    service.ErrBeePlatformAlreadyExists,
			constraint: "uq_bee_platform_active_bee_platform",
		},
		{
			name:       "account already bound",
			err:        &pq.Error{Code: "23505", Constraint: "uq_bee_platform_active_account"},
			wantErr:    service.ErrBeePlatformAccountAlreadyBound,
			constraint: "uq_bee_platform_active_account",
		},
		{
			name:       "wrapped account conflict",
			err:        fmt.Errorf("wrapped: %w", &pq.Error{Code: "23505", Constraint: "uq_bee_platform_active_account"}),
			wantErr:    service.ErrBeePlatformAccountAlreadyBound,
			constraint: "uq_bee_platform_active_account",
		},
		{
			name: "ent constraint message preserves account index",
			err: fmt.Errorf(
				`duplicate key value violates unique constraint "uq_bee_platform_active_account"`,
			),
			wantErr:    service.ErrBeePlatformAccountAlreadyBound,
			constraint: "uq_bee_platform_active_account",
		},
		{
			name:    "unknown unique constraint uses platform conflict",
			err:     &pq.Error{Code: "23505", Constraint: "unknown_constraint"},
			wantErr: service.ErrBeePlatformAlreadyExists,
		},
		{
			name:    "non unique error is preserved",
			err:     &pq.Error{Code: "23503", Constraint: "bee_platform_bee_id_fkey"},
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := translateBeePlatformPersistenceError(tt.err, nil)
			if tt.wantErr == nil {
				require.ErrorIs(t, got, tt.err)
			} else {
				require.ErrorIs(t, got, tt.wantErr)
			}
			if tt.constraint != "" {
				require.Equal(t, tt.constraint, persistenceConstraintName(tt.err))
			}
		})
	}
}
