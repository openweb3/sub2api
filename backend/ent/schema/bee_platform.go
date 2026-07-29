package schema

import (
	"encoding/hex"
	"fmt"
	"strings"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// BeePlatform holds the schema definition for one upstream platform shared by a Bee.
type BeePlatform struct {
	ent.Schema
}

func (BeePlatform) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "bee_platform"},
	}
}

func (BeePlatform) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (BeePlatform) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("bee_id"),
		field.String("platform").
			MaxLen(50).
			NotEmpty().
			Validate(validateBeePlatform),
		field.String("upstream_account_key").
			MaxLen(64).
			NotEmpty().
			Validate(validateUpstreamAccountKey),
		field.Int16("identity_version").
			Default(1),
		field.String("subscription_tier").
			MaxLen(50).
			Optional().
			Nillable(),
		field.Int("concurrency").
			Positive(),
		field.JSON("quota_snapshot", map[string]any{}).
			Default(func() map[string]any { return map[string]any{} }).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
		field.Time("quota_updated_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("last_task_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("status").
			MaxLen(20).
			NotEmpty().
			Validate(validateBeePlatformStatus),
		field.JSON("extra", map[string]any{}).
			Default(func() map[string]any { return map[string]any{} }).
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
	}
}

func (BeePlatform) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("bee", Bee.Type).
			Ref("platforms").
			Field("bee_id").
			Unique().
			Required(),
	}
}

func (BeePlatform) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("bee_id"),
		index.Fields("platform"),
		index.Fields("status"),
		index.Fields("deleted_at"),
		index.Fields("bee_id", "platform").
			Unique().
			Annotations(entsql.IndexWhere("deleted_at IS NULL")),
		index.Fields("platform", "upstream_account_key").
			Unique().
			Annotations(entsql.IndexWhere("deleted_at IS NULL")),
	}
}

func validateBeePlatform(platform string) error {
	switch platform {
	case domain.PlatformOpenAI,
		domain.PlatformAnthropic,
		domain.PlatformGemini,
		domain.PlatformGrok:
		return nil
	default:
		return fmt.Errorf("platform %q is not supported by Bee", platform)
	}
}

func validateUpstreamAccountKey(key string) error {
	if len(key) != 64 || key != strings.ToLower(key) {
		return fmt.Errorf("upstream account key must be a 64-character lowercase SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(key); err != nil {
		return fmt.Errorf("upstream account key must be a 64-character lowercase SHA-256 hex digest")
	}
	return nil
}

func validateBeePlatformStatus(status string) error {
	switch status {
	case domain.StatusActive, domain.StatusDisabled:
		return nil
	default:
		return fmt.Errorf("status %q is not supported by BeePlatform", status)
	}
}
