package protocol

// File transfer rides on a session of kind KindFiles. After the usual
// "paired" notice, the browser sends JSON text frames with one operation at a
// time and the agent answers; file contents travel in binary frames.
//
//	list  {op:"list", path}                      -> {op:"list", path, entries}
//	get   {op:"get", path}                       -> {op:"get", path, size}, binary..., {op:"done", bytes}
//	put   {op:"put", path, size, overwrite}      -> {op:"ready"}; binary...; {op:"end"} -> {op:"done", bytes}
//	mkdir {op:"mkdir", path, parents}            -> {op:"done"}
//
// With parents set, put creates the missing folders of the destination and
// mkdir creates the whole path and accepts a folder that already exists (like
// mkdir -p); this is what lets a whole folder tree be uploaded.
//
// Any step can be answered with {op:"error", error}.
const (
	// KindFiles marks a start-session request as a file transfer channel.
	KindFiles = "files"
	// CapFiles is advertised by agents that can serve file transfer channels.
	CapFiles = "files"

	OpList  = "list"
	OpGet   = "get"
	OpPut   = "put"
	OpMkdir = "mkdir"
	OpReady = "ready"
	OpEnd   = "end"
	OpDone  = "done"
	OpError = "error"
)

// FileMessage is a text frame of the file protocol.
type FileMessage struct {
	Op        string      `json:"op"`
	Path      string      `json:"path,omitempty"`
	Size      int64       `json:"size,omitempty"`
	Overwrite bool        `json:"overwrite,omitempty"`
	Parents   bool        `json:"parents,omitempty"`
	Bytes     int64       `json:"bytes,omitempty"`
	Entries   []FileEntry `json:"entries,omitempty"`
	Error     string      `json:"error,omitempty"`
}

// FileEntry is one item of a directory listing.
type FileEntry struct {
	Name    string `json:"name"`
	Dir     bool   `json:"dir,omitempty"`
	Link    bool   `json:"link,omitempty"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode,omitempty"`
	ModTime int64  `json:"mtime"` // unix seconds
}
