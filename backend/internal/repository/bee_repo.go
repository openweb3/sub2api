package repository

import (
	"context"
	"fmt"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/bee"
	"github.com/Wei-Shaw/sub2api/ent/beeplatform"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

type beeRepository struct {
	client *dbent.Client
}

func NewBeeRepository(client *dbent.Client) service.BeeRepository {
	return &beeRepository{client: client}
}

func (r *beeRepository) Create(ctx context.Context, record *service.Bee) error {
	if record == nil {
		return service.ErrBeeInputRequired
	}

	created, err := clientFromContext(ctx, r.client).Bee.Create().
		SetUserID(record.UserID).
		SetDeviceID(record.DeviceID).
		SetName(record.Name).
		SetStatus(record.Status).
		SetCredentialHash(record.CredentialHash).
		SetCredentialCreatedAt(record.CredentialCreatedAt).
		SetNillableAppVersion(record.AppVersion).
		SetNillableLastConnectedAt(record.LastConnectedAt).
		SetNillableLastDisconnectedAt(record.LastDisconnectedAt).
		SetNillableLastSeenAt(record.LastSeenAt).
		Save(ctx)
	if err != nil {
		return translatePersistenceError(err, nil, service.ErrBeeDeviceAlreadyRegistered)
	}

	*record = *beeEntityToService(created)
	return nil
}

func (r *beeRepository) GetByID(ctx context.Context, id int64) (*service.Bee, error) {
	entity, err := clientFromContext(ctx, r.client).Bee.Query().
		Where(bee.IDEQ(id)).
		Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrBeeNotFound, nil)
	}
	return beeEntityToService(entity), nil
}

func (r *beeRepository) GetByIDAndUserID(ctx context.Context, id, userID int64) (*service.Bee, error) {
	entity, err := clientFromContext(ctx, r.client).Bee.Query().
		Where(
			bee.IDEQ(id),
			bee.UserIDEQ(userID),
		).
		Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrBeeNotFound, nil)
	}
	return beeEntityToService(entity), nil
}

func (r *beeRepository) GetByDeviceID(ctx context.Context, deviceID uuid.UUID) (*service.Bee, error) {
	entity, err := clientFromContext(ctx, r.client).Bee.Query().
		Where(bee.DeviceIDEQ(deviceID)).
		Only(ctx)
	if err != nil {
		return nil, translatePersistenceError(err, service.ErrBeeNotFound, nil)
	}
	return beeEntityToService(entity), nil
}

func (r *beeRepository) ListByUserID(ctx context.Context, userID int64) ([]service.Bee, error) {
	entities, err := clientFromContext(ctx, r.client).Bee.Query().
		Where(bee.UserIDEQ(userID)).
		Order(dbent.Asc(bee.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	records := make([]service.Bee, 0, len(entities))
	for _, entity := range entities {
		records = append(records, *beeEntityToService(entity))
	}
	return records, nil
}

// Update changes mutable Bee properties. UserID and DeviceID are intentionally
// not updated because ownership/device reassignment is not a normal mutation.
func (r *beeRepository) Update(ctx context.Context, userID int64, record *service.Bee) error {
	if record == nil {
		return service.ErrBeeInputRequired
	}

	builder := clientFromContext(ctx, r.client).Bee.UpdateOneID(record.ID).
		Where(
			bee.UserIDEQ(userID),
			bee.DeletedAtIsNil(),
		).
		SetName(record.Name).
		SetStatus(record.Status).
		SetCredentialHash(record.CredentialHash).
		SetCredentialCreatedAt(record.CredentialCreatedAt)
	if record.AppVersion != nil {
		builder.SetAppVersion(*record.AppVersion)
	} else {
		builder.ClearAppVersion()
	}
	if record.LastConnectedAt != nil {
		builder.SetLastConnectedAt(*record.LastConnectedAt)
	} else {
		builder.ClearLastConnectedAt()
	}
	if record.LastDisconnectedAt != nil {
		builder.SetLastDisconnectedAt(*record.LastDisconnectedAt)
	} else {
		builder.ClearLastDisconnectedAt()
	}
	if record.LastSeenAt != nil {
		builder.SetLastSeenAt(*record.LastSeenAt)
	} else {
		builder.ClearLastSeenAt()
	}

	updated, err := builder.Save(ctx)
	if err != nil {
		return translatePersistenceError(err, service.ErrBeeNotFound, nil)
	}
	*record = *beeEntityToService(updated)
	return nil
}

func (r *beeRepository) Delete(ctx context.Context, userID, id int64) error {
	return r.withTx(ctx, func(txCtx context.Context, client *dbent.Client) error {
		_, err := client.Bee.Query().
			Where(
				bee.IDEQ(id),
				bee.UserIDEQ(userID),
			).
			ForUpdate().
			Only(txCtx)
		if err != nil {
			return translatePersistenceError(err, service.ErrBeeNotFound, nil)
		}

		hasPlatforms, err := client.BeePlatform.Query().
			Where(beeplatform.BeeIDEQ(id)).
			Exist(txCtx)
		if err != nil {
			return err
		}
		if hasPlatforms {
			return service.ErrBeeHasPlatformBindings
		}

		err = client.Bee.DeleteOneID(id).
			Where(bee.UserIDEQ(userID)).
			Exec(txCtx)
		return translatePersistenceError(err, service.ErrBeeNotFound, nil)
	})
}

func (r *beeRepository) withTx(
	ctx context.Context,
	fn func(context.Context, *dbent.Client) error,
) error {
	if tx := dbent.TxFromContext(ctx); tx != nil {
		return fn(ctx, tx.Client())
	}

	tx, err := r.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin bee transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	txCtx := dbent.NewTxContext(ctx, tx)
	if err := fn(txCtx, tx.Client()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit bee transaction: %w", err)
	}
	return nil
}

func beeEntityToService(entity *dbent.Bee) *service.Bee {
	if entity == nil {
		return nil
	}
	return &service.Bee{
		ID:                  entity.ID,
		UserID:              entity.UserID,
		DeviceID:            entity.DeviceID,
		Name:                entity.Name,
		Status:              entity.Status,
		CredentialHash:      entity.CredentialHash,
		CredentialCreatedAt: entity.CredentialCreatedAt,
		AppVersion:          entity.AppVersion,
		LastConnectedAt:     entity.LastConnectedAt,
		LastDisconnectedAt:  entity.LastDisconnectedAt,
		LastSeenAt:          entity.LastSeenAt,
		CreatedAt:           entity.CreatedAt,
		UpdatedAt:           entity.UpdatedAt,
	}
}
