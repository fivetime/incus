package api

// MigrationAttemptPut represents the requested state of an inbound migration attempt.
//
// swagger:model
//
// API extension: migration_attempt_fencing.
type MigrationAttemptPut struct {
	// Requested state (active, aborted or settled)
	// Example: active
	State string `json:"state" yaml:"state"`

	// Resource type bound to the token
	// Example: instance
	ResourceType string `json:"resource_type,omitempty" yaml:"resource_type,omitempty"`

	// Resource name bound to the token
	// Example: c1
	ResourceName string `json:"resource_name,omitempty" yaml:"resource_name,omitempty"`

	// Host ID base reserved for a fixed isolated idmap
	// Example: 1000000
	IDMapBase *int64 `json:"idmap_base,omitempty" yaml:"idmap_base,omitempty"`

	// Size reserved for a fixed isolated idmap
	// Example: 65536
	IDMapSize *int64 `json:"idmap_size,omitempty" yaml:"idmap_size,omitempty"`
}

// MigrationAttempt represents a durable target-side migration fence.
//
// swagger:model
//
// API extension: migration_attempt_fencing.
type MigrationAttempt struct {
	// Attempt token
	// Example: 1721ae08-b6a8-416a-9614-3f89302466e1
	Token string `json:"token" yaml:"token"`

	// Project containing the target resource
	// Example: default
	Project string `json:"project" yaml:"project"`

	// Resource type bound to the token
	// Example: instance
	ResourceType string `json:"resource_type" yaml:"resource_type"`

	// Resource name bound to the token
	// Example: c1
	ResourceName string `json:"resource_name" yaml:"resource_name"`

	// Current attempt state
	// Example: active
	State string `json:"state" yaml:"state"`

	// Whether target creation has started
	// Example: true
	Started bool `json:"started" yaml:"started"`

	// Whether target work and rollback have settled
	// Example: false
	Finished bool `json:"finished" yaml:"finished"`

	// Target operation UUID, when known
	// Example: 7a0c9c7e-e55d-4d36-8e3a-5165708bc640
	OperationUUID string `json:"operation_uuid,omitempty" yaml:"operation_uuid,omitempty"`

	// Host ID base reserved for a fixed isolated idmap
	// Example: 1000000
	IDMapBase *int64 `json:"idmap_base,omitempty" yaml:"idmap_base,omitempty"`

	// Size reserved for a fixed isolated idmap
	// Example: 65536
	IDMapSize *int64 `json:"idmap_size,omitempty" yaml:"idmap_size,omitempty"`

	// Daemon generation that accepted the target create request, when started
	// Example: 1754280000000000000
	//
	// API extension: migration_attempt_reservation_generation
	DaemonStart int64 `json:"daemon_start,omitempty" yaml:"daemon_start,omitempty"`

	// Whether the idmap reservation still fences new attempts on this member
	// Example: true
	//
	// API extension: migration_attempt_reservation_generation
	IDMapActive bool `json:"idmap_active" yaml:"idmap_active"`
}
