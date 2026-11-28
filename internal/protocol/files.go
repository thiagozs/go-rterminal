package protocol

// File transfer rides on a session of kind KindFiles. After the usual
// "paired" notice, the browser sends JSON text frames with one operation at a
// time and the agent answers; file contents travel in binary frames.
//
//	list  {op:"list", path}                      -> {op:"list", path, entries}
//	get   {op:"get", path}                       -> {op:"get", path, size}, binary..., {op:"done", bytes}
//	put   {op:"put", path, size, overwrite}      -> {op:"ready"}; binary...; {op:"end"} -> {op:"done", bytes}
//	mkdir {op:"mkdir", path, parents}            -> {op:"done"}
//	rm    {op:"rm", path, recursive}             -> {op:"done"}
//	mv    {op:"mv", path, to}                    -> {op:"done"}
//	zip   {op:"zip", paths}                      -> {op:"zip"}, binary..., {op:"done", bytes, skipped}
//
// put may carry mtime (unix seconds) and mode (permission bits, e.g. 0755 for
// a script): the file is created with them. zip streams a ZIP archive of the
// files and folders named in paths; what cannot be read is left out and
// counted in skipped. rm, mv and zip are advertised as CapFilesManage.
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
	// CapFilesManage is advertised by agents that also serve rm, mv and zip.
	CapFilesManage = "files-manage"
	// CapFilesCopy is advertised by agents that also serve cp.
	CapFilesCopy = "files-copy"

	OpList  = "list"
	OpGet   = "get"
	OpPut   = "put"
	OpMkdir = "mkdir"
	OpRm    = "rm"
	OpMv    = "mv"
	OpZip   = "zip"
	OpCp    = "cp"
	OpReady = "ready"
	OpEnd   = "end"
	OpDone  = "done"
	OpError = "error"

	// CodeExists is the Code of an error about something that already exists.
	CodeExists = "exists"
)

// FileMessage is a text frame of the file protocol.
type FileMessage struct {
	Op        string      `json:"op"`
	Path      string      `json:"path,omitempty"`
	Size      int64       `json:"size,omitempty"`
	Overwrite bool        `json:"overwrite,omitempty"`
	Parents   bool        `json:"parents,omitempty"`
	Recursive bool        `json:"recursive,omitempty"` // rm: remove a folder with everything in it
	To        string      `json:"to,omitempty"`        // mv: the new path
	Paths     []string    `json:"paths,omitempty"`     // zip: what to archive
	Mtime     int64       `json:"mtime,omitempty"`     // put: modification time to set (unix seconds)
	Mode      uint32      `json:"mode,omitempty"`      // put: permission bits to set (0 = default)
	Skipped   int         `json:"skipped,omitempty"`   // zip: items that could not be read
	Bytes     int64       `json:"bytes,omitempty"`
	Entries   []FileEntry `json:"entries,omitempty"`
	Error     string      `json:"error,omitempty"`
	// Code classifies an error for programs ("exists": the target is already there).
	Code string `json:"code,omitempty"`
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
