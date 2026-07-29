package schema

import (
	"fmt"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/google/uuid"
)

// Bee holds the schema definition for a Bee app installation.
type Bee struct {
	ent.Schema
}

func (Bee) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "bee"},
	}
}

func (Bee) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
		mixins.SoftDeleteMixin{},
	}
}

func (Bee) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("user_id"),
		field.UUID("device_id", uuid.UUID{}).
			Unique(),
		field.String("name").
			MaxLen(100).
			NotEmpty(),
		field.String("status").
			MaxLen(20).
			NotEmpty().
			Validate(validateBeeStatus),
		field.String("credential_hash").
			NotEmpty(),
		field.Time("credential_created_at").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("app_version").
			MaxLen(50).
			Optional().
			Nillable(),
		field.Time("last_connected_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("last_disconnected_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("last_seen_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func validateBeeStatus(status string) error {
	switch status {
	case domain.StatusActive, domain.StatusDisabled, "revoked":
		return nil
	default:
		return fmt.Errorf("status %q is not supported by Bee", status)
	}
}

func (Bee) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("bees").
			Field("user_id").
			Unique().
			Required(),
		edge.To("platforms", BeePlatform.Type),
	}
}

func (Bee) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("user_id"),
		index.Fields("status"),
		index.Fields("deleted_at"),
	}
}
