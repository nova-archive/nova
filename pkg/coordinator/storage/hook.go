package storage

// CommittedRef is the post-commit view passed to product OnCommitted hooks.
type CommittedRef struct {
	CID        string
	Product    string
	Visibility Visibility
}
