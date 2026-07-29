package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/bee"
	"github.com/Wei-Shaw/sub2api/ent/beeplatform"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type beePlatformRepository struct {
	client *dbent.Client
}

func NewBeePlatformRepository(client *dbent.Client) service.BeePlatformRepository {
	return &beePlatformRepository{client: client}
}

func (r *beePlatformRepository) Create(ctx context.Context, record *service.BeePlatform) error {
	if record == nil {
		return service.ErrBeePlatformInputRequired
	}

	identityVersion := record.IdentityVersion
	if identityVersion == 0 {
		identityVersion = 1
	}
	quotaSnapshot := record.QuotaSnapshot
	if quotaSnapshot == nil {
		quotaSnapshot = map[string]any{}
	}
	extra := record.Extra
	if extra == nil {
		extra = map[string]any{}
	}

	var created *dbent.BeePlatform
	err := r.withTx(ctx, func(txCtx context.Context, client *dbent.Client) error {
		_, err := client.Bee.Query().
			Where(bee.IDEQ(record.BeeID)).
			ForUpdate().
			Only(txCtx)
		if err != nil {
			return translatePersistenceError(err, service.ErrBeeNotFound, nil)
		}

		created, err = client.BeePlatform.Create().
			SetBeeID(record.BeeID).
			SetPlatform(record.Platform).
			SetUpstreamAccountKey(record.UpstreamAccountKey).
			SetIdentityVersion(identityVersion).
			SetNillableSubscriptionTier(record.SubscriptionTier).
			SetConcurrency(record.Concurrency).
			SetQuotaSnapshot(quotaSnapshot).
			SetNillableQuotaUpdatedAt(record.QuotaUpdatedAt).
			SetNillableLastTaskAt(record.LastTaskAt).
			SetStatus(record.Status).
			SetExtra(extra).
			Save(txCtx)
		return translateBeePlatformPersistenceError(err, nil)
	})
	if err != nil {
		return err
	}

	*record = *beePlatformEntityToService(created)
	return nil
}

func (r *beePlatformRepository) GetByID(ctx context.Context, id int64) (*service.BeePlatform, error) {
	entity, err := clientFromContext(ctx, r.client).BeePlatform.Query().
		Where(beeplatform.IDEQ(id)).
		Only(ctx)
	if err != nil {
		return nil, translateBeePlatformPersistenceError(err, service.ErrBeePlatformNotFound)
	}
	return beePlatformEntityToService(entity), nil
}

func (r *beePlatformRepository) GetByIDAndBeeID(
	ctx context.Context,
	id int64,
	beeID int64,
) (*service.BeePlatform, error) {
	entity, err := clientFromContext(ctx, r.client).BeePlatform.Query().
		Where(
			beeplatform.IDEQ(id),
			beeplatform.BeeIDEQ(beeID),
		).
		Only(ctx)
	if err != nil {
		return nil, translateBeePlatformPersistenceError(err, service.ErrBeePlatformNotFound)
	}
	return beePlatformEntityToService(entity), nil
}

func (r *beePlatformRepository) GetByBeeAndPlatform(
	ctx context.Context,
	beeID int64,
	platform string,
) (*service.BeePlatform, error) {
	entity, err := clientFromContext(ctx, r.client).BeePlatform.Query().
		Where(
			beeplatform.BeeIDEQ(beeID),
			beeplatform.PlatformEQ(platform),
		).
		Only(ctx)
	if err != nil {
		return nil, translateBeePlatformPersistenceError(err, service.ErrBeePlatformNotFound)
	}
	return beePlatformEntityToService(entity), nil
}

func (r *beePlatformRepository) GetByPlatformAndAccountKey(
	ctx context.Context,
	platform string,
	accountKey string,
) (*service.BeePlatform, error) {
	entity, err := clientFromContext(ctx, r.client).BeePlatform.Query().
		Where(
			beeplatform.PlatformEQ(platform),
			beeplatform.UpstreamAccountKeyEQ(accountKey),
		).
		Only(ctx)
	if err != nil {
		return nil, translateBeePlatformPersistenceError(err, service.ErrBeePlatformNotFound)
	}
	return beePlatformEntityToService(entity), nil
}

func (r *beePlatformRepository) ListByBeeID(ctx context.Context, beeID int64) ([]service.BeePlatform, error) {
	entities, err := clientFromContext(ctx, r.client).BeePlatform.Query().
		Where(beeplatform.BeeIDEQ(beeID)).
		Order(dbent.Asc(beeplatform.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	records := make([]service.BeePlatform, 0, len(entities))
	for _, entity := range entities {
		records = append(records, *beePlatformEntityToService(entity))
	}
	return records, nil
}

// Update changes mutable platform properties. BeeID, Platform,
// UpstreamAccountKey and IdentityVersion are intentionally immutable; moving
// or replacing a binding requires delete + create so the old binding remains
// visible in soft-deleted history.
func (r *beePlatformRepository) Update(ctx context.Context, beeID int64, record *service.BeePlatform) error {
	if record == nil {
		return service.ErrBeePlatformInputRequired
	}

	quotaSnapshot := record.QuotaSnapshot
	if quotaSnapshot == nil {
		quotaSnapshot = map[string]any{}
	}
	extra := record.Extra
	if extra == nil {
		extra = map[string]any{}
	}

	builder := clientFromContext(ctx, r.client).BeePlatform.UpdateOneID(record.ID).
		Where(
			beeplatform.BeeIDEQ(beeID),
			beeplatform.DeletedAtIsNil(),
		).
		SetConcurrency(record.Concurrency).
		SetQuotaSnapshot(quotaSnapshot).
		SetStatus(record.Status).
		SetExtra(extra)
	if record.SubscriptionTier != nil {
		builder.SetSubscriptionTier(*record.SubscriptionTier)
	} else {
		builder.ClearSubscriptionTier()
	}
	if record.QuotaUpdatedAt != nil {
		builder.SetQuotaUpdatedAt(*record.QuotaUpdatedAt)
	} else {
		builder.ClearQuotaUpdatedAt()
	}
	if record.LastTaskAt != nil {
		builder.SetLastTaskAt(*record.LastTaskAt)
	} else {
		builder.ClearLastTaskAt()
	}

	updated, err := builder.Save(ctx)
	if err != nil {
		return translateBeePlatformPersistenceError(err, service.ErrBeePlatformNotFound)
	}
	*record = *beePlatformEntityToService(updated)
	return nil
}

func (r *beePlatformRepository) Delete(ctx context.Context, beeID, id int64) error {
	err := clientFromContext(ctx, r.client).BeePlatform.DeleteOneID(id).
		Where(beeplatform.BeeIDEQ(beeID)).
		Exec(ctx)
	return translateBeePlatformPersistenceError(err, service.ErrBeePlatformNotFound)
}

func (r *beePlatformRepository) withTx(
	ctx context.Context,
	fn func(context.Context, *dbent.Client) error,
) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return fn(ctx, tx.Client())
	}

	tx, err := r.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin bee platform transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	txCtx := dbent.NewTxContext(ctx, tx)
	if err := fn(txCtx, tx.Client()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit bee platform transaction: %w", err)
	}
	return nil
}

func translateBeePlatformPersistenceError(err error, notFound *infraerrors.ApplicationError) error {
	if err == nil {
		return nil
	}
	if notFound != nil && dbent.IsNotFound(err) {
		return notFound.WithCause(err)
	}
	if !isUniqueConstraintViolation(err) {
		return err
	}

	switch persistenceConstraintName(err) {
	case "uq_bee_platform_active_account":
		return service.ErrBeePlatformAccountAlreadyBound.WithCause(err)
	case "uq_bee_platform_active_bee_platform":
		return service.ErrBeePlatformAlreadyExists.WithCause(err)
	default:
		return service.ErrBeePlatformAlreadyExists.WithCause(err)
	}
}

func persistenceConstraintName(err error) string {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		return pgErr.Constraint
	}

	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "uq_bee_platform_active_account"):
		return "uq_bee_platform_active_account"
	case strings.Contains(message, "uq_bee_platform_active_bee_platform"):
		return "uq_bee_platform_active_bee_platform"
	default:
		return ""
	}
}

func beePlatformEntityToService(entity *dbent.BeePlatform) *service.BeePlatform {
	if entity == nil {
		return nil
	}
	return &service.BeePlatform{
		ID:                 entity.ID,
		BeeID:              entity.BeeID,
		Platform:           entity.Platform,
		UpstreamAccountKey: entity.UpstreamAccountKey,
		IdentityVersion:    entity.IdentityVersion,
		SubscriptionTier:   entity.SubscriptionTier,
		Concurrency:        entity.Concurrency,
		QuotaSnapshot:      entity.QuotaSnapshot,
		QuotaUpdatedAt:     entity.QuotaUpdatedAt,
		LastTaskAt:         entity.LastTaskAt,
		Status:             entity.Status,
		Extra:              entity.Extra,
		CreatedAt:          entity.CreatedAt,
		UpdatedAt:          entity.UpdatedAt,
	}
}
