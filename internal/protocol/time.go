package protocol

import "time"

// NowMillis / NowSecs are tiny indirection points so tests can swap in a
// deterministic clock without touching production code.
var (
	NowMillis = func() int64 { return time.Now().UnixMilli() }
	NowSecs   = func() int64 { return time.Now().Unix() }
)
