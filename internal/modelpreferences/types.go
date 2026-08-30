package modelpreferences

import "time"

// Preference controls whether a normalized model selector is shown in the
// personal dashboard. Selector scopes follow modelselectors: exact
// provider/model, provider-wide provider/, model-wide model, and global /.
type Preference struct {
	Selector  string    `json:"selector" bson:"_id"`
	Hidden    bool      `json:"hidden" bson:"hidden"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}