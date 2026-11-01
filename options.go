package smartmem

// FlagNoCache bypasses the MemProcFS cache, so every read sees live memory.
// It equals MemFlagNoCache in gomemprocfs.
const FlagNoCache uint64 = 0x0001

// Option configures a Session.
type Option func(*options)

type options struct {
	flags        uint64
	maxRounds    int
	maxGap       int
	maxStringLen int
}

func defaultOptions() options {
	return options{flags: FlagNoCache, maxRounds: 4096, maxGap: 64, maxStringLen: 4096}
}

// WithFlags sets the MemProcFS read flags. The default is FlagNoCache.
func WithFlags(flags uint64) Option { return func(o *options) { o.flags = flags } }

// WithMaxRounds sets how many rounds one execution may take. Requests still
// pending after that fail with ErrMaxRounds. The default is 4096: enough to
// walk a linked list of a few thousand elements, one round each, while a
// cyclic list still ends with an error.
func WithMaxRounds(n int) Option { return func(o *options) { o.maxRounds = max(1, n) } }

// WithMaxGap sets the largest gap, in bytes, between two reads on the same
// page that are merged into one range. The default is 64.
func WithMaxGap(n int) Option { return func(o *options) { o.maxGap = max(0, n) } }

// WithMaxStringLen caps the bytes read for one UNICODE_STRING. The default is 4096.
func WithMaxStringLen(n int) Option { return func(o *options) { o.maxStringLen = max(2, n) } }
