package schema

import (
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PromptRule 定义提示词注入规则的 schema。
type PromptRule struct {
	ent.Schema
}

func (PromptRule) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "prompt_rules"},
	}
}

func (PromptRule) Mixin() []ent.Mixin {
	return []ent.Mixin{
		mixins.TimeMixin{},
	}
}

func (PromptRule) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").
			MaxLen(200).
			NotEmpty(),

		field.Text("description").
			Optional().
			Nillable(),

		field.Bool("enabled").
			Default(true),

		field.Int("order").
			Default(0),

		field.String("role").
			MaxLen(20).
			Default("system"),

		field.Text("content").
			NotEmpty(),

		field.String("action").
			MaxLen(10).
			Default("prepend"),

		field.Text("match_pattern").
			Optional().
			Nillable(),

		field.String("match_mode").
			MaxLen(10).
			Default("plain").
			Optional(),

		field.JSON("group_ids", []int64{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),

		field.JSON("model_ids", []string{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),
	}
}

func (PromptRule) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("enabled"),
		index.Fields("order"),
	}
}
