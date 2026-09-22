package postgres

// Events remains only as a constructor compatibility shim for tests and older
// in-process adapters. Production delivery uses the transactional outbox and
// Redis-backed realtime hub; PostgreSQL notification transport was removed.
type Events struct{}

func NewEvents(_ any, _ any) *Events { return &Events{} }

func (*Events) Subscribe(string) (<-chan struct{}, func()) {
	changes := make(chan struct{})
	return changes, func() {}
}
