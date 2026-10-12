package secrets

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/redentordev/tako-cli/pkg/config"
)

// RedactionLabelsEnv is the environment variable that turns marker labels
// off: `off`, `0`, `false` or `no` (case-insensitive) render every mask as
// the plain `[REDACTED]`. Masking itself cannot be disabled.
const RedactionLabelsEnv = "TAKO_REDACTION_LABELS"

// RedactionLabelsEnabled reports whether TAKO_REDACTION_LABELS leaves
// labels on.
func RedactionLabelsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(RedactionLabelsEnv))) {
	case "off", "0", "false", "no":
		return false
	}
	return true
}

// ShouldRedactEnvValue reports whether a plain service `env:` value should be
// registered with the redactor. Values under credential-like names always
// are. Other values only when they look like a credential: a URL with
// embedded userinfo or a token-like path segment or query value (webhook
// URLs), or a long single token mixing letters and digits. A credential-like
// locator name (`BETTER_AUTH_URL`) whose value is a plain URL or host list
// stays visible, and a lowercase dotted host name is token-shaped only when
// one of its labels is. Ordinary settings such as `CMS_RUNTIME_MODE: site` or `NODE_ENV:
// production` would otherwise mask every occurrence of that word, including
// inside service names like `site-renderer`. Values under `secrets:` and
// `secretSources` are registered separately and are always redacted.
func ShouldRedactEnvValue(name, value string) bool {
	return ClassifyEnvValue(name, value).Masked()
}

// ClassifyEnvValue returns why a plain `env:` or envFile value is masked or
// stays visible; see ShouldRedactEnvValue.
func ClassifyEnvValue(name, value string) RedactionReason {
	if value == "" {
		return ReasonEmpty
	}
	if config.IsCredentialEnvName(name) {
		// A locator such as BETTER_AUTH_URL or OAUTH_ISSUER_URL names a
		// credential-like subject, but its value is an address that appears
		// in domain and URL output; it stays visible unless it carries
		// credentials.
		if isLocatorEnvName(name) {
			if reason, ok := classifyLocatorValue(value); ok {
				return reason
			}
		}
		return ReasonCredentialName
	}
	if _, _, ok := strings.Cut(value, "://"); ok {
		if reason := classifyURL(value); reason != ReasonNone {
			return reason
		}
	}
	if isHostName(value) {
		if hostLooksLikeCredential(value) {
			return ReasonTokenShapedValue
		}
		return ReasonPlainSetting
	}
	if looksLikeCredential(value) {
		return ReasonTokenShapedValue
	}
	return ReasonPlainSetting
}

// hostLooksLikeCredential reports whether any dot-separated label of a host
// name is token-shaped, such as a random subdomain used as a secret.
func hostLooksLikeCredential(host string) bool {
	host, _, _ = strings.Cut(host, ":")
	for _, label := range strings.Split(host, ".") {
		if looksLikeCredential(label) {
			return true
		}
	}
	return false
}

// classifyURL classifies a `scheme://...` value: masked when it has userinfo
// or a token-like path or query part, otherwise a plain setting. It returns
// ReasonNone when value is not a URL.
func classifyURL(value string) RedactionReason {
	scheme, rest, ok := strings.Cut(value, "://")
	if !ok || scheme == "" {
		return ReasonNone
	}
	host, path, _ := strings.Cut(rest, "/")
	if strings.Contains(host, "@") {
		return ReasonURLWithCredentials
	}
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '?' || r == '&' || r == '=' || r == '#'
	}) {
		if looksLikeCredential(part) {
			return ReasonTokenURLPart
		}
	}
	return ReasonPlainSetting
}

// locatorEnvSuffixes are name suffixes whose values are addresses, not
// credentials.
var locatorEnvSuffixes = []string{
	"_URL", "_URLS", "_URI", "_URIS", "_HOST", "_HOSTS", "_HOSTNAME",
	"_ORIGIN", "_ORIGINS", "_ENDPOINT", "_ISSUER", "_DOMAIN", "_DOMAINS",
}

func isLocatorEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, suffix := range locatorEnvSuffixes {
		if strings.HasSuffix(upper, suffix) {
			return true
		}
	}
	return false
}

// classifyLocatorValue classifies the value of a locator-named variable: a
// list of URLs or host names separated by commas or spaces. It reports
// ok=false when any item is neither, so the caller keeps the name-based mask.
// A URL with credentials or a token-like part, or a token-shaped host, is
// masked.
func classifyLocatorValue(value string) (RedactionReason, bool) {
	items := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(items) == 0 {
		return ReasonNone, false
	}
	for _, item := range items {
		if reason := classifyURL(item); reason != ReasonNone {
			if reason.Masked() {
				return reason, true
			}
			continue
		}
		if !isHostName(item) {
			return ReasonNone, false
		}
		if hostLooksLikeCredential(item) {
			return ReasonTokenShapedValue, true
		}
	}
	return ReasonPlainSetting, true
}

// isHostName reports whether value is a dotted lowercase host name,
// optionally with a port, such as `admin.example.com` or `db.internal:5432`.
// Mixed case is rejected so a dotted random token is not mistaken for one.
func isHostName(value string) bool {
	host, port, hasPort := strings.Cut(value, ":")
	if hasPort {
		if port == "" {
			return false
		}
		for _, r := range port {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	if !strings.Contains(host, ".") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, r := range host {
		if !(r == '.' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

func looksLikeCredential(value string) bool {
	if len(value) < 16 {
		return false
	}
	var letters, digits bool
	for _, r := range value {
		switch {
		case unicode.IsSpace(r):
			return false
		case unicode.IsLetter(r):
			letters = true
		case unicode.IsDigit(r):
			digits = true
		}
	}
	return letters && digits
}

// redactPattern is one heuristic secret pattern. group selects the submatch
// that holds the secret; zero means the whole match. Text before the group
// (the key of a key/value match) is kept.
type redactPattern struct {
	id    string
	re    *regexp.Regexp
	group int
}

// Pattern IDs rendered as `pattern.<id>`.
const (
	PatternAssignment        = "assignment"
	PatternConnectionString  = "connection-string"
	PatternStripeKey         = "stripe-key"
	PatternGitHubToken       = "github-token"
	PatternNPMToken          = "npm-token"
	PatternDigitalOceanToken = "digitalocean-token"
	PatternAWSAccessKey      = "aws-access-key"
	PatternGoogleAPIKey      = "google-api-key"
	PatternHex64             = "hex64"
	PatternJWT               = "jwt"
	PatternBase64            = "base64"
	PatternHex               = "hex"
)

// PatternIDs lists every heuristic pattern ID in match order, deduplicated.
func PatternIDs() []string {
	return []string{
		PatternAssignment,
		PatternConnectionString,
		PatternStripeKey,
		PatternGitHubToken,
		PatternNPMToken,
		PatternDigitalOceanToken,
		PatternAWSAccessKey,
		PatternGoogleAPIKey,
		PatternHex64,
		PatternJWT,
		PatternBase64,
		PatternHex,
	}
}

// Redactor automatically redacts sensitive information from output.
//
// Registered values are replaced by a marker naming their source,
// `[REDACTED:<label>]`, or `[REDACTED]` when unlabeled or when
// TAKO_REDACTION_LABELS turns labels off. Redact is deterministic and
// idempotent: Redact(Redact(x)) == Redact(x).
type Redactor struct {
	mu        sync.RWMutex
	secrets   map[string]string   // value -> rendered label
	labels    map[string]struct{} // every label registered, for marker protection
	public    map[string]struct{}
	patterns  []redactPattern
	minLength int
	labelsOn  bool

	// Sorted views of secrets and public values, rebuilt lazily after
	// registration changes.
	dirty       bool
	secretOrder []string
	publicOrder []string
}

// NewRedactor creates a new redactor. It reads TAKO_REDACTION_LABELS once.
func NewRedactor() *Redactor {
	return &Redactor{
		secrets:   make(map[string]string),
		labels:    make(map[string]struct{}),
		public:    make(map[string]struct{}),
		patterns:  compilePatterns(),
		minLength: 4, // Don't redact very short values
		labelsOn:  RedactionLabelsEnabled(),
	}
}

// compilePatterns compiles regex patterns for sensitive data detection
func compilePatterns() []redactPattern {
	specs := []struct {
		id      string
		pattern string
		group   int
	}{
		// Environment variable assignments; the key is kept.
		{PatternAssignment, `(?i)(password|passwd|pwd|pass)[\s=:]+([^\s]+)`, 2},
		{PatternAssignment, `(?i)(secret|token|key|apikey|api_key|api-key)[\s=:]+([^\s]+)`, 2},
		{PatternAssignment, `(?i)(auth|authorization|bearer|credential)[\s=:]+([^\s]+)`, 2},

		// Connection strings; only the password is masked.
		{PatternConnectionString, `(?i)mysql:\/\/[^:]+:([^@]+)@`, 1},
		{PatternConnectionString, `(?i)postgres(ql)?:\/\/[^:]+:([^@]+)@`, 2},
		{PatternConnectionString, `(?i)mongodb(\+srv)?:\/\/[^:]+:([^@]+)@`, 2},
		{PatternConnectionString, `(?i)redis:\/\/[^:]+:([^@]+)@`, 1},

		// API keys and tokens
		{PatternStripeKey, `sk_live_[A-Za-z0-9]+`, 0},
		{PatternStripeKey, `pk_live_[A-Za-z0-9]+`, 0},
		{PatternGitHubToken, `ghp_[A-Za-z0-9]{36}`, 0},
		{PatternGitHubToken, `ghs_[A-Za-z0-9]{36}`, 0},
		{PatternGitHubToken, `github_pat_[A-Za-z0-9_]+`, 0},
		{PatternNPMToken, `npm_[A-Za-z0-9]{36}`, 0},
		{PatternDigitalOceanToken, `dop_[A-Za-z0-9]{32}`, 0},

		// Cloud provider patterns
		{PatternAWSAccessKey, `AKIA[0-9A-Z]{16}`, 0},
		{PatternGoogleAPIKey, `AIza[0-9A-Za-z\-_]{35}`, 0},
		{PatternDigitalOceanToken, `dop_v1_[a-f0-9]{64}`, 0},
		{PatternHex64, `[a-f0-9]{64}`, 0}, // Hetzner/Linode tokens

		// JWT tokens
		{PatternJWT, `eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`, 0},

		// Base64 encoded secrets (40+ characters)
		{PatternBase64, `[A-Za-z0-9+/]{40,}={0,2}`, 0},

		// Hex encoded secrets (32+ characters)
		{PatternHex, `[a-f0-9]{32,}`, 0},
	}

	patterns := make([]redactPattern, 0, len(specs))
	for _, spec := range specs {
		if compiled, err := regexp.Compile(spec.pattern); err == nil {
			patterns = append(patterns, redactPattern{id: spec.id, re: compiled, group: spec.group})
		}
	}
	return patterns
}

// Register marks a specific value for redaction without a label; it renders
// as `[REDACTED]`. Prefer RegisterLabeled.
func (r *Redactor) Register(value string) {
	r.RegisterLabeled(value, "")
}

// RegisterLabeled marks a value for redaction under a source label (see the
// label constructors in labels.go). When the same value is registered under
// several labels, the label of the highest-precedence kind wins, ties broken
// lexicographically, independent of registration order.
func (r *Redactor) RegisterLabeled(value, label string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// NUL bytes are protected input during Redact, so a value containing one
	// is registered as its NUL-free parts; each part is still masked.
	if strings.Contains(value, "\x00") {
		for _, part := range strings.Split(value, "\x00") {
			r.registerLocked(part, label)
		}
		return
	}
	r.registerLocked(value, label)
}

func (r *Redactor) registerLocked(value, label string) {
	// Skip very short values
	if len(value) < r.minLength {
		return
	}
	label = SanitizeLabel(label)
	if label != "" {
		r.labels[label] = struct{}{}
	}
	current, exists := r.secrets[value]
	if !exists || preferLabel(label, current) {
		r.secrets[value] = label
	}
	r.dirty = true
}

// Labels returns every label registered with this redactor, sorted.
func (r *Redactor) Labels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	labels := make([]string, 0, len(r.labels))
	for label := range r.labels {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	return labels
}

// LabelsEnabled reports whether markers carry labels.
func (r *Redactor) LabelsEnabled() bool {
	return r.labelsOn
}

// RegisterPublic marks a value that is known not to be secret, such as a
// project, service or environment name, an image reference, or a git commit.
// A registered secret that lies entirely inside an occurrence of a longer
// public value stays readable, and patterns never match a public value. A
// public value never shields a secret that contains or overlaps it.
func (r *Redactor) RegisterPublic(value string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// A value with a NUL byte can never match protected input.
	if len(value) < r.minLength || strings.Contains(value, "\x00") {
		return
	}
	r.public[value] = struct{}{}
	r.dirty = true
}

// marker renders the mask for a label.
func (r *Redactor) marker(label string) string {
	if label == "" || !r.labelsOn {
		return "[REDACTED]"
	}
	return "[REDACTED:" + label + "]"
}

// markerPattern matches text shaped like a marker; only markers this
// redactor could have produced are protected (see protectMarkers).
var markerPattern = regexp.MustCompile(`\[REDACTED(?::([A-Za-z0-9_\-@/:.]{1,96}))?\]`)

// slot is one protected span swapped out while redaction runs.
type slot struct {
	text   string
	marker bool // a marker, as opposed to a public value or an input NUL
}

// workText is the text being redacted. Each protected span (a marker, a
// public value, or a NUL byte from the input) is a single NUL byte in text,
// and slots holds the spans in the order of their NUL bytes. Every NUL in
// text is a slot, so input can never forge a placeholder, and restore only
// emits text this call protected.
type workText struct {
	text  string
	slots []slot
}

// newWorkText protects every NUL byte of the input as itself.
func newWorkText(input string) *workText {
	w := &workText{text: input}
	if count := strings.Count(input, "\x00"); count > 0 {
		w.slots = make([]slot, count)
		for i := range w.slots {
			w.slots[i] = slot{text: "\x00"}
		}
	}
	return w
}

// edit replaces text[start:end] with one protected slot.
type edit struct {
	start, end int
	slot       slot
}

// apply performs edits, sorted by start and non-overlapping, in one sweep.
// Slots of NUL bytes inside a replaced span are dropped with it.
func (w *workText) apply(edits []edit) {
	if len(edits) == 0 {
		return
	}
	var b strings.Builder
	b.Grow(len(w.text))
	slots := make([]slot, 0, len(w.slots)+len(edits))
	next, last := 0, 0
	for _, e := range edits {
		kept := strings.Count(w.text[last:e.start], "\x00")
		slots = append(slots, w.slots[next:next+kept]...)
		next += kept + strings.Count(w.text[e.start:e.end], "\x00")
		b.WriteString(w.text[last:e.start])
		b.WriteByte(0)
		slots = append(slots, e.slot)
		last = e.end
	}
	slots = append(slots, w.slots[next:]...)
	b.WriteString(w.text[last:])
	w.text, w.slots = b.String(), slots
}

// restore swaps every slot back in.
func (w *workText) restore() string {
	if len(w.slots) == 0 {
		return w.text
	}
	var b strings.Builder
	text := w.text
	for _, s := range w.slots {
		index := strings.IndexByte(text, 0)
		if index < 0 {
			break
		}
		b.WriteString(text[:index])
		b.WriteString(s.text)
		text = text[index+1:]
	}
	b.WriteString(text)
	return b.String()
}

// rebuild refreshes the sorted views. Callers hold the write lock.
func (r *Redactor) rebuild() {
	r.secretOrder = r.secretOrder[:0]
	for value := range r.secrets {
		r.secretOrder = append(r.secretOrder, value)
	}
	sortLongestFirst(r.secretOrder)
	r.publicOrder = r.publicOrder[:0]
	for value := range r.public {
		if _, isSecret := r.secrets[value]; isSecret {
			continue
		}
		r.publicOrder = append(r.publicOrder, value)
	}
	sortLongestFirst(r.publicOrder)
	r.dirty = false
}

func sortLongestFirst(values []string) {
	sort.Slice(values, func(i, j int) bool {
		if len(values[i]) != len(values[j]) {
			return len(values[i]) > len(values[j])
		}
		return values[i] < values[j]
	})
}

// Redact removes sensitive information from text.
//
// It runs in order:
//  0. NUL bytes in the input are protected as themselves, so input text can
//     never be mistaken for an internal placeholder.
//  1. Markers this redactor could have produced are swapped for placeholders.
//  2. One pass over registered secrets and public values: a secret occurrence
//     stays readable only inside a longer public occurrence; every other
//     secret occurrence is masked (longest secret first, overlapping
//     occurrences merged into one marker); public occurrences that do not
//     overlap a masked secret are protected.
//  3. Heuristic patterns run on the result; markers and protected public
//     values are placeholders and are never matched.
//  4. Placeholders are restored.
//
// Every step is a single sweep over sorted occurrences, so the cost is
// near-linear in the input size and the number of occurrences.
func (r *Redactor) Redact(input string) string {
	r.mu.RLock()
	if r.dirty {
		r.mu.RUnlock()
		r.mu.Lock()
		if r.dirty {
			r.rebuild()
		}
		r.mu.Unlock()
		r.mu.RLock()
	}
	defer r.mu.RUnlock()

	work := newWorkText(input)
	r.protectMarkers(work)
	r.redactRegistered(work)
	for _, pattern := range r.patterns {
		r.redactPatternMatches(pattern, work)
	}
	return work.restore()
}

// protectMarkers swaps markers this redactor could have produced for
// placeholders: the bare `[REDACTED]`, markers with a registered label, and
// `[REDACTED:pattern.<id>]` for a known pattern ID. Other marker-shaped text
// is ordinary input, so it can never shield a secret.
func (r *Redactor) protectMarkers(work *workText) {
	if !strings.Contains(work.text, "[REDACTED") {
		return
	}
	var edits []edit
	for _, match := range markerPattern.FindAllStringIndex(work.text, -1) {
		marker := work.text[match[0]:match[1]]
		if marker == "[REDACTED]" || r.ownsLabel(marker[len("[REDACTED:"):len(marker)-1]) {
			edits = append(edits, edit{start: match[0], end: match[1], slot: slot{text: marker, marker: true}})
		}
	}
	work.apply(edits)
}

func (r *Redactor) ownsLabel(label string) bool {
	if _, ok := r.labels[label]; ok {
		return true
	}
	if id, ok := strings.CutPrefix(label, LabelKindPattern+"."); ok {
		for _, known := range PatternIDs() {
			if id == known {
				return true
			}
		}
	}
	return false
}

type occurrence struct {
	start, end int
	priority   int // index in the longest-first order
	label      string
}

func findOccurrences(input string, values []string, labels map[string]string) []occurrence {
	var found []occurrence
	for priority, value := range values {
		offset := 0
		for {
			index := strings.Index(input[offset:], value)
			if index < 0 {
				break
			}
			start := offset + index
			occ := occurrence{start: start, end: start + len(value), priority: priority}
			if labels != nil {
				occ.label = labels[value]
			}
			found = append(found, occ)
			offset = start + len(value)
		}
	}
	return found
}

func overlaps(a, b occurrence) bool {
	return a.start < b.end && b.start < a.end
}

// redactRegistered masks registered secrets and protects public values in
// one deterministic pass (see Redact).
func (r *Redactor) redactRegistered(work *workText) {
	if len(r.secretOrder) == 0 && len(r.publicOrder) == 0 {
		return
	}
	input := work.text
	secretOcc := findOccurrences(input, r.secretOrder, r.secrets)
	publicOcc := findOccurrences(input, r.publicOrder, nil)
	if len(secretOcc) == 0 && len(publicOcc) == 0 {
		return
	}
	masked, validPublic := resolveShields(secretOcc, publicOcc)

	// Merge overlapping masked occurrences; each merged span takes the label
	// of its highest-priority (longest) secret.
	var spans []occurrence
	for i, secret := range secretOcc {
		if masked[i] {
			spans = append(spans, secret)
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].priority < spans[j].priority
	})
	var merged []occurrence
	for _, span := range spans {
		if n := len(merged); n > 0 && span.start < merged[n-1].end {
			last := &merged[n-1]
			if span.end > last.end {
				last.end = span.end
			}
			if span.priority < last.priority {
				last.priority = span.priority
				last.label = span.label
			}
			continue
		}
		merged = append(merged, span)
	}

	// Protect valid public occurrences, longest first, without overlaps.
	// publicOcc is already in priority order (findOccurrences walks values
	// longest first), and chosen spans never overlap, so marking covered
	// bytes keeps the overlap check linear overall.
	var chosen []occurrence
	var covered []bool
	for j, public := range publicOcc {
		if !validPublic[j] {
			continue
		}
		if covered == nil {
			covered = make([]bool, len(input))
		}
		free := true
		for k := public.start; k < public.end; k++ {
			if covered[k] {
				free = false
				break
			}
		}
		if !free {
			continue
		}
		for k := public.start; k < public.end; k++ {
			covered[k] = true
		}
		chosen = append(chosen, public)
	}

	edits := make([]edit, 0, len(merged)+len(chosen))
	for _, span := range merged {
		edits = append(edits, edit{start: span.start, end: span.end, slot: slot{text: r.marker(span.label), marker: true}})
	}
	for _, public := range chosen {
		edits = append(edits, edit{start: public.start, end: public.end, slot: slot{text: input[public.start:public.end]}})
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	work.apply(edits)
}

// resolveShields decides which secret occurrences are masked and which public
// occurrences stay valid. A secret occurrence is masked unless a valid public
// occurrence strictly longer than it contains it; a public occurrence is
// invalid once it overlaps a masked secret. Starting from every public valid,
// a worklist propagates both rules to their fixpoint. Each secret is masked
// and each public invalidated at most once, and every lookup scans only the
// occurrences whose start lies within one value length of the span, so the
// cost is near-linear rather than quadratic in the occurrence counts.
func resolveShields(secrets, publics []occurrence) (masked, validPublic []bool) {
	masked = make([]bool, len(secrets))
	validPublic = make([]bool, len(publics))
	for i := range validPublic {
		validPublic[i] = true
	}
	if len(publics) == 0 {
		for i := range masked {
			masked[i] = true
		}
		return masked, validPublic
	}

	secretByStart := occurrencesByStart(secrets)
	publicByStart := occurrencesByStart(publics)
	maxPublic := 0
	for _, public := range publics {
		maxPublic = max(maxPublic, public.end-public.start)
	}
	shields := func(public, secret occurrence) bool {
		return public.end-public.start > secret.end-secret.start &&
			public.start <= secret.start && secret.end <= public.end
	}

	// shieldCount[i] is the number of valid publics shielding secret i.
	shieldCount := make([]int, len(secrets))
	for i, secret := range secrets {
		forEachStartIn(publics, publicByStart, secret.end-maxPublic, secret.start+1, func(j int) {
			if shields(publics[j], secret) {
				shieldCount[i]++
			}
		})
	}
	var queue []int
	for i := range secrets {
		if shieldCount[i] == 0 {
			masked[i] = true
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		secret := secrets[queue[len(queue)-1]]
		queue = queue[:len(queue)-1]
		forEachStartIn(publics, publicByStart, secret.start-maxPublic+1, secret.end, func(j int) {
			public := publics[j]
			if !validPublic[j] || !overlaps(public, secret) {
				return
			}
			validPublic[j] = false
			forEachStartIn(secrets, secretByStart, public.start, public.end, func(k int) {
				if masked[k] || !shields(public, secrets[k]) {
					return
				}
				shieldCount[k]--
				if shieldCount[k] == 0 {
					masked[k] = true
					queue = append(queue, k)
				}
			})
		})
	}
	return masked, validPublic
}

// occurrencesByStart returns the indexes of occs sorted by start.
func occurrencesByStart(occs []occurrence) []int {
	order := make([]int, len(occs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return occs[order[a]].start < occs[order[b]].start })
	return order
}

// forEachStartIn calls fn with the index of every occurrence whose start is
// in [lo, hi).
func forEachStartIn(occs []occurrence, byStart []int, lo, hi int, fn func(int)) {
	first := sort.Search(len(byStart), func(i int) bool { return occs[byStart[i]].start >= lo })
	for i := first; i < len(byStart) && occs[byStart[i]].start < hi; i++ {
		fn(byStart[i])
	}
}

// redactPatternMatches applies one pattern, leaving hex image tags and
// digests (`repo:<hex>`, `sha256:<hex>`) alone: those are commit SHAs and
// content hashes, not credentials. Key/value patterns run first, so a hex
// secret written as `token: <hex>` is still redacted. A matched value that is
// only marker placeholders plus closing punctuation is left as is, which
// keeps markers intact and redaction idempotent. A protected public value is
// not a marker: `POSTGRES_PASSWORD=postgres` is masked even when `postgres`
// names a service.
func (r *Redactor) redactPatternMatches(pattern redactPattern, work *workText) {
	input := work.text
	matches := pattern.re.FindAllStringSubmatchIndex(input, -1)
	if len(matches) == 0 {
		return
	}
	var edits []edit
	slotIndex, counted := 0, 0 // slotIndex is the number of NULs in input[:counted]
	for _, match := range matches {
		start, end := match[0], match[1]
		valueStart, valueEnd := start, end
		if pattern.group > 0 && 2*pattern.group+1 < len(match) && match[2*pattern.group] >= 0 {
			valueStart, valueEnd = match[2*pattern.group], match[2*pattern.group+1]
		}
		slotIndex += strings.Count(input[counted:valueStart], "\x00")
		counted = valueStart
		value := input[valueStart:valueEnd]
		if isHexReference(input, start, input[start:end]) || onlyMarkers(value, work.slots[slotIndex:]) {
			continue
		}
		edits = append(edits, edit{start: valueStart, end: valueEnd, slot: slot{text: r.marker(PatternLabel(pattern.id)), marker: true}})
	}
	work.apply(edits)
}

// onlyMarkers reports whether value holds at least one placeholder, every
// placeholder in it is a marker (slots starts at value's first placeholder),
// and nothing else but closing punctuation.
func onlyMarkers(value string, slots []slot) bool {
	count := strings.Count(value, "\x00")
	if count == 0 || count > len(slots) {
		return false
	}
	for _, s := range slots[:count] {
		if !s.marker {
			return false
		}
	}
	rest := strings.ReplaceAll(value, "\x00", "")
	return strings.Trim(rest, "\"'`,;.)]}>") == ""
}

func isHexReference(input string, start int, value string) bool {
	if start == 0 || input[start-1] != ':' || !isLowerHex(value) {
		return false
	}
	return len(value) == 40 || len(value) == 64
}

func isLowerHex(value string) bool {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return value != ""
}

// RedactMap redacts sensitive values in a map
func (r *Redactor) RedactMap(data map[string]string) map[string]string {
	redacted := make(map[string]string, len(data))

	for key, value := range data {
		if r.IsSensitiveKey(key) {
			redacted[key] = "[REDACTED]"
		} else {
			redacted[key] = r.Redact(value)
		}
	}

	return redacted
}

// IsSensitiveKey checks if a key name indicates sensitive data
func (r *Redactor) IsSensitiveKey(key string) bool {
	sensitiveKeywords := []string{
		"PASSWORD", "PASSWD", "PWD",
		"SECRET", "TOKEN", "KEY",
		"API", "APIKEY", "API_KEY",
		"AUTH", "AUTHORIZATION", "BEARER",
		"CREDENTIAL", "CRED",
		"PRIVATE", "CERT", "CERTIFICATE",
		"SIGNING", "ENCRYPTION",
	}

	keyUpper := strings.ToUpper(key)
	for _, keyword := range sensitiveKeywords {
		if strings.Contains(keyUpper, keyword) {
			return true
		}
	}

	return false
}

// Clear removes all registered secrets
func (r *Redactor) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.secrets = make(map[string]string)
	r.labels = make(map[string]struct{})
	r.dirty = true
}
