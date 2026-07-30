package service

import (
	"context"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const (
	BeeStatusActive   = "active"
	BeeStatusDisabled = "disabled"
	BeeStatusRevoked  = "revoked"

	BeePlatformStatusActive   = "active"
	BeePlatformStatusDisabled = "disabled"
)

var (
	ErrBeeNotFound                = infraerrors.NotFound("BEE_NOT_FOUND", "bee not found")
	ErrBeeInputRequired           = infraerrors.BadRequest("BEE_INPUT_REQUIRED", "bee input is required")
	ErrBeeDeviceAlreadyRegistered = infraerrors.Conflict(
		"BEE_DEVICE_ALREADY_REGISTERED",
		"bee device is already registered",
	)
	ErrBeeHasPlatformBindings = infraerrors.Conflict(
		"BEE_HAS_PLATFORM_BINDINGS",
		"bee has platform bindings; unbind them before deleting the bee",
	)
	ErrBeePlatformNotFound      = infraerrors.NotFound("BEE_PLATFORM_NOT_FOUND", "bee platform not found")
	ErrBeePlatformInputRequired = infraerrors.BadRequest(
		"BEE_PLATFORM_INPUT_REQUIRED",
		"bee platform input is required",
	)
	ErrBeePlatformAlreadyExists = infraerrors.Conflict(
		"BEE_PLATFORM_ALREADY_EXISTS",
		"bee already shares this platform",
	)
	ErrBeePlatformAccountAlreadyBound = infraerrors.Conflict(
		"BEE_PLATFORM_ACCOUNT_ALREADY_BOUND",
		"platform account is already bound to another bee",
	)
)

// Bee represents one registered Bee app installation.
type Bee struct {
	ID                  int64
	UserID              int64
	DeviceID            uuid.UUID
	Name                string
	Status              string
	CredentialHash      string `json:"-"`
	CredentialCreatedAt time.Time
	AppVersion          *string
	LastConnectedAt     *time.Time
	LastDisconnectedAt  *time.Time
	LastSeenAt          *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// BeePlatform represents one upstream platform shared by a Bee.
type BeePlatform struct {
	ID                 int64
	BeeID              int64
	Platform           string
	UpstreamAccountKey string
	IdentityVersion    int16
	SubscriptionTier   *string
	Concurrency        int
	QuotaSnapshot      map[string]any
	QuotaUpdatedAt     *time.Time
	LastTaskAt         *time.Time
	Status             string
	Extra              map[string]any
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type BeeRepository interface {
	Create(ctx context.Context, bee *Bee) error
	GetByID(ctx context.Context, id int64) (*Bee, error)
	GetByIDAndUserID(ctx context.Context, id, userID int64) (*Bee, error)
	GetByDeviceID(ctx context.Context, deviceID uuid.UUID) (*Bee, error)
	ListByUserID(ctx context.Context, userID int64) ([]Bee, error)
	Update(ctx context.Context, userID int64, bee *Bee) error
	Delete(ctx context.Context, userID, id int64) error
}

type BeePlatformRepository interface {
	Create(ctx context.Context, platform *BeePlatform) error
	GetByID(ctx context.Context, id int64) (*BeePlatform, error)
	GetByIDAndBeeID(ctx context.Context, id, beeID int64) (*BeePlatform, error)
	GetByBeeAndPlatform(ctx context.Context, beeID int64, platform string) (*BeePlatform, error)
	GetByPlatformAndAccountKey(ctx context.Context, platform, accountKey string) (*BeePlatform, error)
	ListByBeeID(ctx context.Context, beeID int64) ([]BeePlatform, error)
	Update(ctx context.Context, beeID int64, platform *BeePlatform) error
	Delete(ctx context.Context, beeID, id int64) error
}
