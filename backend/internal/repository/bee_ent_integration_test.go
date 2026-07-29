//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/bee"
	"github.com/Wei-Shaw/sub2api/ent/beeplatform"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestBeeEnt_CreateAndQueryRelationships(t *testing.T) {
	ctx := context.Background()
	entClient := testEntTx(t).Client()
	now := time.Now().UTC().Truncate(time.Microsecond)

	owner, err := entClient.User.Create().
		SetEmail(uniqueTestValue(t, "bee-owner") + "@example.com").
		SetPasswordHash("test-password-hash").
		Save(ctx)
	require.NoError(t, err)

	createdBee, err := entClient.Bee.Create().
		SetUserID(owner.ID).
		SetDeviceID(uuid.New()).
		SetName("test-bee").
		SetStatus(domain.StatusActive).
		SetCredentialHash("hashed-test-credential").
		SetCredentialCreatedAt(now).
		Save(ctx)
	require.NoError(t, err)

	createdPlatform, err := entClient.BeePlatform.Create().
		SetBeeID(createdBee.ID).
		SetPlatform(domain.PlatformOpenAI).
		SetUpstreamAccountKey(testUpstreamAccountKey(uuid.NewString())).
		SetSubscriptionTier("plus").
		SetConcurrency(3).
		SetStatus(domain.StatusActive).
		SetQuotaSnapshot(map[string]any{"remaining": float64(42)}).
		SetQuotaUpdatedAt(now).
		SetExtra(map[string]any{"region": "test"}).
		Save(ctx)
	require.NoError(t, err)

	queriedBee, err := entClient.Bee.Query().
		Where(bee.ID(createdBee.ID)).
		WithUser().
		WithPlatforms().
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, owner.ID, queriedBee.Edges.User.ID)
	require.Len(t, queriedBee.Edges.Platforms, 1)
	require.Equal(t, createdPlatform.ID, queriedBee.Edges.Platforms[0].ID)

	queriedPlatform, err := entClient.BeePlatform.Query().
		Where(beeplatform.ID(createdPlatform.ID)).
		WithBee().
		Only(ctx)
	require.NoError(t, err)
	require.Equal(t, createdBee.ID, queriedPlatform.Edges.Bee.ID)
	require.Equal(t, int16(1), queriedPlatform.IdentityVersion)
	require.Equal(t, float64(42), queriedPlatform.QuotaSnapshot["remaining"])

	queriedOwner, err := entClient.User.Query().
		Where(user.ID(owner.ID)).
		WithBees().
		Only(ctx)
	require.NoError(t, err)
	require.Len(t, queriedOwner.Edges.Bees, 1)
	require.Equal(t, createdBee.ID, queriedOwner.Edges.Bees[0].ID)
}

func TestBeePlatformEnt_RejectsUnsupportedPlatform(t *testing.T) {
	ctx := context.Background()
	entClient := testEntTx(t).Client()

	owner, err := entClient.User.Create().
		SetEmail(uniqueTestValue(t, "bee-platform-owner") + "@example.com").
		SetPasswordHash("test-password-hash").
		Save(ctx)
	require.NoError(t, err)

	createdBee, err := entClient.Bee.Create().
		SetUserID(owner.ID).
		SetDeviceID(uuid.New()).
		SetName("test-bee").
		SetStatus(domain.StatusActive).
		SetCredentialHash("hashed-test-credential").
		SetCredentialCreatedAt(time.Now()).
		Save(ctx)
	require.NoError(t, err)

	_, err = entClient.BeePlatform.Create().
		SetBeeID(createdBee.ID).
		SetPlatform("unsupported").
		SetUpstreamAccountKey(testUpstreamAccountKey(uuid.NewString())).
		SetConcurrency(1).
		SetStatus(domain.StatusActive).
		Save(ctx)
	require.ErrorContains(t, err, "is not supported by Bee")
}

func TestBeePlatformEnt_RejectsDuplicatePlatformOnSameBee(t *testing.T) {
	ctx := context.Background()
	entClient := testEntTx(t).Client()
	createdBee := createTestBee(t, ctx, entClient, "duplicate-platform")

	_, err := entClient.BeePlatform.Create().
		SetBeeID(createdBee.ID).
		SetPlatform(domain.PlatformOpenAI).
		SetUpstreamAccountKey(testUpstreamAccountKey("first-account")).
		SetConcurrency(1).
		SetStatus(domain.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	_, err = entClient.BeePlatform.Create().
		SetBeeID(createdBee.ID).
		SetPlatform(domain.PlatformOpenAI).
		SetUpstreamAccountKey(testUpstreamAccountKey("second-account")).
		SetConcurrency(1).
		SetStatus(domain.StatusActive).
		Save(ctx)
	require.Error(t, err)
}

func TestBeePlatformEnt_RejectsAccountBoundToAnotherBee(t *testing.T) {
	ctx := context.Background()
	entClient := testEntTx(t).Client()
	firstBee := createTestBee(t, ctx, entClient, "first-account-owner")
	secondBee := createTestBee(t, ctx, entClient, "second-account-owner")
	accountKey := testUpstreamAccountKey("shared-account")

	_, err := entClient.BeePlatform.Create().
		SetBeeID(firstBee.ID).
		SetPlatform(domain.PlatformOpenAI).
		SetUpstreamAccountKey(accountKey).
		SetConcurrency(1).
		SetStatus(domain.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	_, err = entClient.BeePlatform.Create().
		SetBeeID(secondBee.ID).
		SetPlatform(domain.PlatformOpenAI).
		SetUpstreamAccountKey(accountKey).
		SetConcurrency(1).
		SetStatus(domain.StatusActive).
		Save(ctx)
	require.Error(t, err)
}

func TestBeePlatformEnt_AllowsReuseAfterSoftDelete(t *testing.T) {
	ctx := context.Background()
	entClient := testEntTx(t).Client()
	createdBee := createTestBee(t, ctx, entClient, "soft-delete-reuse")
	accountKey := testUpstreamAccountKey("reusable-account")

	createdPlatform, err := entClient.BeePlatform.Create().
		SetBeeID(createdBee.ID).
		SetPlatform(domain.PlatformOpenAI).
		SetUpstreamAccountKey(accountKey).
		SetConcurrency(1).
		SetStatus(domain.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	require.NoError(t, entClient.BeePlatform.DeleteOneID(createdPlatform.ID).Exec(ctx))

	replacement, err := entClient.BeePlatform.Create().
		SetBeeID(createdBee.ID).
		SetPlatform(domain.PlatformOpenAI).
		SetUpstreamAccountKey(accountKey).
		SetConcurrency(1).
		SetStatus(domain.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	require.NotEqual(t, createdPlatform.ID, replacement.ID)
}

func createTestBee(t *testing.T, ctx context.Context, entClient *dbent.Client, name string) *dbent.Bee {
	t.Helper()

	owner, err := entClient.User.Create().
		SetEmail(uniqueTestValue(t, name) + "@example.com").
		SetPasswordHash("test-password-hash").
		Save(ctx)
	require.NoError(t, err)

	createdBee, err := entClient.Bee.Create().
		SetUserID(owner.ID).
		SetDeviceID(uuid.New()).
		SetName(name).
		SetStatus(domain.StatusActive).
		SetCredentialHash("hashed-test-credential").
		SetCredentialCreatedAt(time.Now()).
		Save(ctx)
	require.NoError(t, err)
	return createdBee
}

func testUpstreamAccountKey(seed string) string {
	sum := sha256.Sum256([]byte("tokenhive:v1:" + seed))
	return hex.EncodeToString(sum[:])
}
