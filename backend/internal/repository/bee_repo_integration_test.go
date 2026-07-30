//go:build integration

package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestBeeRepository_CRUDAndOwnerQueries(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()
	repo := NewBeeRepository(client)
	ownerID := createBeeRepositoryOwner(t, ctx, client, "bee-repo-owner")
	now := time.Now().UTC().Truncate(time.Microsecond)
	appVersion := "1.0.0"

	record := &service.Bee{
		UserID:              ownerID,
		DeviceID:            uuid.New(),
		Name:                "desktop",
		Status:              service.BeeStatusActive,
		CredentialHash:      "credential-hash",
		CredentialCreatedAt: now,
		AppVersion:          &appVersion,
	}
	require.NoError(t, repo.Create(ctx, record))
	require.NotZero(t, record.ID)
	require.False(t, record.CreatedAt.IsZero())

	byID, err := repo.GetByID(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, record.DeviceID, byID.DeviceID)
	require.Equal(t, ownerID, byID.UserID)

	byOwner, err := repo.GetByIDAndUserID(ctx, record.ID, ownerID)
	require.NoError(t, err)
	require.Equal(t, record.ID, byOwner.ID)

	byDevice, err := repo.GetByDeviceID(ctx, record.DeviceID)
	require.NoError(t, err)
	require.Equal(t, record.ID, byDevice.ID)

	list, err := repo.ListByUserID(ctx, ownerID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, record.ID, list[0].ID)

	lastSeen := now.Add(time.Minute)
	originalUserID := record.UserID
	originalDeviceID := record.DeviceID
	record.Name = "renamed"
	record.Status = service.BeeStatusDisabled
	record.LastSeenAt = &lastSeen
	record.UserID++
	record.DeviceID = uuid.New()
	require.NoError(t, repo.Update(ctx, ownerID, record))

	updated, err := repo.GetByID(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, originalUserID, updated.UserID)
	require.Equal(t, originalDeviceID, updated.DeviceID)
	require.Equal(t, "renamed", updated.Name)
	require.Equal(t, service.BeeStatusDisabled, updated.Status)
	require.Equal(t, lastSeen, *updated.LastSeenAt)

	require.NoError(t, repo.Delete(ctx, ownerID, record.ID))
	_, err = repo.GetByID(ctx, record.ID)
	require.ErrorIs(t, err, service.ErrBeeNotFound)
	require.ErrorIs(t, repo.Update(ctx, ownerID, record), service.ErrBeeNotFound)
	require.ErrorIs(t, repo.Delete(ctx, ownerID, record.ID), service.ErrBeeNotFound)
}

func TestBeeRepository_MapsDeviceConflict(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()
	repo := NewBeeRepository(client)
	ownerID := createBeeRepositoryOwner(t, ctx, client, "bee-device-conflict")
	deviceID := uuid.New()

	first := newBeeRepositoryRecord(ownerID, deviceID, "first")
	require.NoError(t, repo.Create(ctx, first))

	second := newBeeRepositoryRecord(ownerID, deviceID, "second")
	err := repo.Create(ctx, second)
	require.ErrorIs(t, err, service.ErrBeeDeviceAlreadyRegistered)
}

func TestBeeRepository_DeleteRequiresPlatformsUnboundAndManagesOwnTransaction(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	beeRepo := NewBeeRepository(client)
	platformRepo := NewBeePlatformRepository(client)
	ownerID := createBeeRepositoryOwner(t, ctx, client, "delete-bound-bee-owner")
	beeRecord := newBeeRepositoryRecord(ownerID, uuid.New(), "bound")
	require.NoError(t, beeRepo.Create(ctx, beeRecord))

	platformRecord := newBeePlatformRepositoryRecord(
		beeRecord.ID,
		domain.PlatformOpenAI,
		testUpstreamAccountKey("delete-bound-bee-account"),
	)
	require.NoError(t, platformRepo.Create(ctx, platformRecord))

	require.ErrorIs(
		t,
		beeRepo.Delete(ctx, ownerID, beeRecord.ID),
		service.ErrBeeHasPlatformBindings,
	)
	_, err := beeRepo.GetByID(ctx, beeRecord.ID)
	require.NoError(t, err)
	_, err = platformRepo.GetByPlatformAndAccountKey(
		ctx,
		platformRecord.Platform,
		platformRecord.UpstreamAccountKey,
	)
	require.NoError(t, err)

	require.NoError(t, platformRepo.Delete(ctx, beeRecord.ID, platformRecord.ID))
	require.NoError(t, beeRepo.Delete(ctx, ownerID, beeRecord.ID))
	_, err = beeRepo.GetByID(ctx, beeRecord.ID)
	require.ErrorIs(t, err, service.ErrBeeNotFound)
}

func TestBeePlatformRepository_CRUDAndBindingQueries(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()
	beeRepo := NewBeeRepository(client)
	platformRepo := NewBeePlatformRepository(client)
	ownerID := createBeeRepositoryOwner(t, ctx, client, "bee-platform-repo-owner")
	beeRecord := newBeeRepositoryRecord(ownerID, uuid.New(), "platform-owner")
	require.NoError(t, beeRepo.Create(ctx, beeRecord))
	now := time.Now().UTC().Truncate(time.Microsecond)
	tier := "plus"

	record := &service.BeePlatform{
		BeeID:              beeRecord.ID,
		Platform:           domain.PlatformOpenAI,
		UpstreamAccountKey: testUpstreamAccountKey("repository-account"),
		IdentityVersion:    1,
		SubscriptionTier:   &tier,
		Concurrency:        3,
		QuotaSnapshot:      map[string]any{"remaining": float64(10)},
		QuotaUpdatedAt:     &now,
		Status:             service.BeePlatformStatusActive,
		Extra:              map[string]any{"region": "test"},
	}
	require.NoError(t, platformRepo.Create(ctx, record))
	require.NotZero(t, record.ID)

	byID, err := platformRepo.GetByID(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, record.UpstreamAccountKey, byID.UpstreamAccountKey)

	byBeePlatform, err := platformRepo.GetByBeeAndPlatform(ctx, beeRecord.ID, domain.PlatformOpenAI)
	require.NoError(t, err)
	require.Equal(t, record.ID, byBeePlatform.ID)

	byAccount, err := platformRepo.GetByPlatformAndAccountKey(
		ctx,
		domain.PlatformOpenAI,
		record.UpstreamAccountKey,
	)
	require.NoError(t, err)
	require.Equal(t, record.ID, byAccount.ID)

	list, err := platformRepo.ListByBeeID(ctx, beeRecord.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)

	record.Concurrency = 5
	record.Status = service.BeePlatformStatusDisabled
	record.QuotaSnapshot = map[string]any{"remaining": float64(8)}
	originalBeeID := record.BeeID
	originalPlatform := record.Platform
	originalAccountKey := record.UpstreamAccountKey
	originalIdentityVersion := record.IdentityVersion
	record.BeeID++
	record.Platform = domain.PlatformAnthropic
	record.UpstreamAccountKey = testUpstreamAccountKey("replacement-account")
	record.IdentityVersion++
	require.NoError(t, platformRepo.Update(ctx, beeRecord.ID, record))

	updated, err := platformRepo.GetByID(ctx, record.ID)
	require.NoError(t, err)
	require.Equal(t, originalBeeID, updated.BeeID)
	require.Equal(t, originalPlatform, updated.Platform)
	require.Equal(t, originalAccountKey, updated.UpstreamAccountKey)
	require.Equal(t, originalIdentityVersion, updated.IdentityVersion)
	require.Equal(t, 5, updated.Concurrency)
	require.Equal(t, service.BeePlatformStatusDisabled, updated.Status)
	require.Equal(t, float64(8), updated.QuotaSnapshot["remaining"])

	require.NoError(t, platformRepo.Delete(ctx, beeRecord.ID, record.ID))
	_, err = platformRepo.GetByID(ctx, record.ID)
	require.ErrorIs(t, err, service.ErrBeePlatformNotFound)
	require.ErrorIs(
		t,
		platformRepo.Update(ctx, beeRecord.ID, record),
		service.ErrBeePlatformNotFound,
	)
	require.ErrorIs(
		t,
		platformRepo.Delete(ctx, beeRecord.ID, record.ID),
		service.ErrBeePlatformNotFound,
	)

	replacement := newBeePlatformRepositoryRecord(
		beeRecord.ID,
		domain.PlatformOpenAI,
		originalAccountKey,
	)
	require.NoError(t, platformRepo.Create(ctx, replacement))
	require.NotEqual(t, record.ID, replacement.ID)
}

func TestBeeRepositories_RejectCrossOwnerAndCrossBeeMutations(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()
	beeRepo := NewBeeRepository(client)
	platformRepo := NewBeePlatformRepository(client)
	ownerID := createBeeRepositoryOwner(t, ctx, client, "scoped-owner")
	otherOwnerID := createBeeRepositoryOwner(t, ctx, client, "other-owner")
	firstBee := newBeeRepositoryRecord(ownerID, uuid.New(), "first")
	secondBee := newBeeRepositoryRecord(ownerID, uuid.New(), "second")
	require.NoError(t, beeRepo.Create(ctx, firstBee))
	require.NoError(t, beeRepo.Create(ctx, secondBee))

	_, err := beeRepo.GetByIDAndUserID(ctx, firstBee.ID, otherOwnerID)
	require.ErrorIs(t, err, service.ErrBeeNotFound)
	require.ErrorIs(t, beeRepo.Update(ctx, otherOwnerID, firstBee), service.ErrBeeNotFound)
	require.ErrorIs(t, beeRepo.Delete(ctx, otherOwnerID, firstBee.ID), service.ErrBeeNotFound)

	platformRecord := newBeePlatformRepositoryRecord(
		firstBee.ID,
		domain.PlatformOpenAI,
		testUpstreamAccountKey("scoped-account"),
	)
	require.NoError(t, platformRepo.Create(ctx, platformRecord))
	_, err = platformRepo.GetByIDAndBeeID(ctx, platformRecord.ID, secondBee.ID)
	require.ErrorIs(t, err, service.ErrBeePlatformNotFound)
	require.ErrorIs(
		t,
		platformRepo.Update(ctx, secondBee.ID, platformRecord),
		service.ErrBeePlatformNotFound,
	)
	require.ErrorIs(
		t,
		platformRepo.Delete(ctx, secondBee.ID, platformRecord.ID),
		service.ErrBeePlatformNotFound,
	)
}

func TestBeePlatformRepository_RejectsSoftDeletedBee(t *testing.T) {
	tx := testEntTx(t)
	ctx := dbent.NewTxContext(context.Background(), tx)
	client := tx.Client()
	beeRepo := NewBeeRepository(client)
	platformRepo := NewBeePlatformRepository(client)
	ownerID := createBeeRepositoryOwner(t, ctx, client, "deleted-bee-owner")
	beeRecord := newBeeRepositoryRecord(ownerID, uuid.New(), "deleted")
	require.NoError(t, beeRepo.Create(ctx, beeRecord))
	require.NoError(t, beeRepo.Delete(ctx, ownerID, beeRecord.ID))

	platformRecord := newBeePlatformRepositoryRecord(
		beeRecord.ID,
		domain.PlatformOpenAI,
		testUpstreamAccountKey("deleted-bee-account"),
	)
	require.ErrorIs(t, platformRepo.Create(ctx, platformRecord), service.ErrBeeNotFound)
}

func TestBeePlatformRepository_CreateManagesOwnTransaction(t *testing.T) {
	ctx := context.Background()
	client := testEntClient(t)
	beeRepo := NewBeeRepository(client)
	platformRepo := NewBeePlatformRepository(client)
	ownerID := createBeeRepositoryOwner(t, ctx, client, "own-transaction-owner")
	beeRecord := newBeeRepositoryRecord(ownerID, uuid.New(), "own-transaction")
	require.NoError(t, beeRepo.Create(ctx, beeRecord))

	openAIRecord := newBeePlatformRepositoryRecord(
		beeRecord.ID,
		domain.PlatformOpenAI,
		testUpstreamAccountKey(uniqueTestValue(t, "own-transaction-openai")),
	)
	require.NoError(t, platformRepo.Create(ctx, openAIRecord))

	committed, err := platformRepo.GetByID(ctx, openAIRecord.ID)
	require.NoError(t, err)
	require.Equal(t, openAIRecord.ID, committed.ID)

	conflict := newBeePlatformRepositoryRecord(
		beeRecord.ID,
		domain.PlatformOpenAI,
		testUpstreamAccountKey(uniqueTestValue(t, "own-transaction-conflict")),
	)
	require.ErrorIs(t, platformRepo.Create(ctx, conflict), service.ErrBeePlatformAlreadyExists)

	// A failed transaction must be rolled back without poisoning the base client.
	anthropicRecord := newBeePlatformRepositoryRecord(
		beeRecord.ID,
		domain.PlatformAnthropic,
		testUpstreamAccountKey(uniqueTestValue(t, "own-transaction-anthropic")),
	)
	require.NoError(t, platformRepo.Create(ctx, anthropicRecord))
}

func TestBeePlatformRepository_MapsDistinctBindingConflicts(t *testing.T) {
	tests := []struct {
		name       string
		buildOther func(firstBeeID, secondBeeID int64, accountKey string) *service.BeePlatform
		wantErr    error
	}{
		{
			name: "same bee and platform",
			buildOther: func(firstBeeID, _ int64, _ string) *service.BeePlatform {
				return newBeePlatformRepositoryRecord(
					firstBeeID,
					domain.PlatformOpenAI,
					testUpstreamAccountKey("other-account"),
				)
			},
			wantErr: service.ErrBeePlatformAlreadyExists,
		},
		{
			name: "same platform account on another bee",
			buildOther: func(_, secondBeeID int64, accountKey string) *service.BeePlatform {
				return newBeePlatformRepositoryRecord(secondBeeID, domain.PlatformOpenAI, accountKey)
			},
			wantErr: service.ErrBeePlatformAccountAlreadyBound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tx := testEntTx(t)
			ctx := dbent.NewTxContext(context.Background(), tx)
			client := tx.Client()
			beeRepo := NewBeeRepository(client)
			platformRepo := NewBeePlatformRepository(client)
			ownerID := createBeeRepositoryOwner(t, ctx, client, "binding-conflict")
			firstBee := newBeeRepositoryRecord(ownerID, uuid.New(), "first")
			secondBee := newBeeRepositoryRecord(ownerID, uuid.New(), "second")
			require.NoError(t, beeRepo.Create(ctx, firstBee))
			require.NoError(t, beeRepo.Create(ctx, secondBee))

			accountKey := testUpstreamAccountKey("bound-account")
			first := newBeePlatformRepositoryRecord(firstBee.ID, domain.PlatformOpenAI, accountKey)
			require.NoError(t, platformRepo.Create(ctx, first))

			err := platformRepo.Create(ctx, tt.buildOther(firstBee.ID, secondBee.ID, accountKey))
			require.True(t, errors.Is(err, tt.wantErr), "got %v, want %v", err, tt.wantErr)
		})
	}
}

func createBeeRepositoryOwner(t *testing.T, ctx context.Context, client *dbent.Client, name string) int64 {
	t.Helper()
	owner, err := client.User.Create().
		SetEmail(uniqueTestValue(t, name) + "@example.com").
		SetPasswordHash("test-password-hash").
		Save(ctx)
	require.NoError(t, err)
	return owner.ID
}

func newBeeRepositoryRecord(userID int64, deviceID uuid.UUID, name string) *service.Bee {
	return &service.Bee{
		UserID:              userID,
		DeviceID:            deviceID,
		Name:                name,
		Status:              service.BeeStatusActive,
		CredentialHash:      "credential-hash",
		CredentialCreatedAt: time.Now(),
	}
}

func newBeePlatformRepositoryRecord(beeID int64, platform, accountKey string) *service.BeePlatform {
	return &service.BeePlatform{
		BeeID:              beeID,
		Platform:           platform,
		UpstreamAccountKey: accountKey,
		IdentityVersion:    1,
		Concurrency:        1,
		Status:             service.BeePlatformStatusActive,
	}
}
