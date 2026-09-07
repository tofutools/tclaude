package model

// DirectoryListing is a bounded, live view of an explicitly requested host directory.
// It is not a filesystem authority grant or a snapshot for later execution.
type DirectoryListing struct {
	Path        string
	Parent      string
	Directories []DirectoryEntry
	NextAfter   string `json:",omitempty"`
}
type DirectoryEntry struct {
	Name string
	Path string
}
