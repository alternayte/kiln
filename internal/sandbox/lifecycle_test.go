package sandbox

import (
	"testing"
	"time"

	"github.com/alternayte/kiln/internal/store"
)

// TestNextDeadline pins the timer rule: one timer per sandbox, at the first
// of the idle deadline and the ttl deadline.
func TestNextDeadline(t *testing.T) {
	created := time.Unix(1_700_000_000, 0).UTC()
	active := created.Add(100 * time.Second)
	ttl := func(seconds int) *int { return &seconds }
	base := store.Sandbox{
		ID:           "abc",
		Lifecycle:    store.LifecycleEphemeral,
		State:        store.SandboxRunning,
		IdleSeconds:  300,
		LastActiveAt: active,
		CreatedAt:    created,
	}
	cases := []struct {
		name string
		row  store.Sandbox
		want time.Time
		ok   bool
	}{
		{
			name: "ephemeral sleeps never, dies at idle",
			row:  base,
			want: active.Add(300 * time.Second),
			ok:   true,
		},
		{
			name: "ephemeral dies at the earlier ttl",
			row: func() store.Sandbox {
				row := base
				row.TTLSeconds = ttl(50)
				return row
			}(),
			want: created.Add(50 * time.Second),
			ok:   true,
		},
		{
			name: "persistent sleeps at idle",
			row: func() store.Sandbox {
				row := base
				row.Lifecycle = store.LifecyclePersistent
				return row
			}(),
			want: active.Add(300 * time.Second),
			ok:   true,
		},
		{
			name: "persistent dies at the earlier ttl",
			row: func() store.Sandbox {
				row := base
				row.Lifecycle = store.LifecyclePersistent
				row.TTLSeconds = ttl(50)
				return row
			}(),
			want: created.Add(50 * time.Second),
			ok:   true,
		},
		{
			name: "sleeping persistent waits for ttl",
			row: func() store.Sandbox {
				row := base
				row.Lifecycle = store.LifecyclePersistent
				row.State = store.SandboxSleeping
				row.TTLSeconds = ttl(600)
				return row
			}(),
			want: created.Add(600 * time.Second),
			ok:   true,
		},
		{
			name: "sleeping without ttl has no deadline",
			row: func() store.Sandbox {
				row := base
				row.Lifecycle = store.LifecyclePersistent
				row.State = store.SandboxSleeping
				return row
			}(),
			ok: false,
		},
		{
			name: "creating is owned by its request",
			row: func() store.Sandbox {
				row := base
				row.State = store.SandboxCreating
				return row
			}(),
			ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := nextDeadline(tc.row)
			if ok != tc.ok {
				t.Fatalf("nextDeadline ok = %t, want %t", ok, tc.ok)
			}
			if ok && !got.Equal(tc.want) {
				t.Fatalf("nextDeadline = %s, want %s", got, tc.want)
			}
		})
	}
}
