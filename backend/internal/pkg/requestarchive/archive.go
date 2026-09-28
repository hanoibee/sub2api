// requestarchive 为每个网关请求保存一次上游调用的归档副本。
package requestarchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var archiveLocation = time.FixedZone("UTC+08", 8*60*60)

// FailureReasonSequenceConflict is the bounded metric label used when an
// allocated staging directory already exists.
const FailureReasonSequenceConflict = "sequence_conflict"

type sequenceAllocator struct {
	mu       sync.Mutex
	date     string
	sequence uint64
}

var archiveAllocators = struct {
	sync.RWMutex
	values map[string]*sequenceAllocator
}{values: make(map[string]*sequenceAllocator)}

var sequenceConflictFailures atomic.Uint64

type Config struct {
	Root         string
	ProviderCode string
	// PackagedRoot is the root containing provider/date/batch_manifest.json.
	// It is primarily exposed for tests; production archives use /maasData/archive.
	PackagedRoot string
}

const defaultPackagedArchiveRoot = "/maasData/archive"

type batchManifest struct {
	Date         string `json:"date"`
	ProviderCode string `json:"provider_code"`
	Batches      []struct {
		LastRequest string `json:"last_request"`
	} `json:"batches"`
}

// Initialize validates an enabled archive root and initializes its in-memory
// sequence from the current day's completed and staging directories. Call it
// during process startup; request handling never performs directory scans.
func Initialize(cfg Config, now time.Time) error {
	root := strings.TrimSpace(cfg.Root)
	if root == "" {
		return errors.New("request archive directory is required")
	}
	root = filepath.Clean(root)
	localTime := now.In(archiveLocation)
	dateDir := localTime.Format("2006-01-02")
	dateID := localTime.Format("20060102")
	finalParent := filepath.Join(root, dateDir)
	stagingParent := filepath.Join(root, ".staging", dateDir)
	if err := os.MkdirAll(finalParent, 0700); err != nil {
		return fmt.Errorf("create request archive directory: %w", err)
	}
	if err := os.MkdirAll(stagingParent, 0700); err != nil {
		return fmt.Errorf("create request archive staging directory: %w", err)
	}
	if err := validateArchiveFilesystem(stagingParent, finalParent); err != nil {
		return err
	}
	sequence, err := highestArchiveSequence("req_"+dateID+"_", finalParent, stagingParent)
	if err != nil {
		return err
	}
	packagedSequence, err := highestPackagedSequence(cfg, dateDir, dateID)
	if err != nil {
		return err
	}
	if packagedSequence > sequence {
		sequence = packagedSequence
	}

	archiveAllocators.Lock()
	archiveAllocators.values[root] = &sequenceAllocator{date: dateID, sequence: sequence}
	archiveAllocators.Unlock()
	return nil
}

func highestPackagedSequence(cfg Config, dateDir, dateID string) (uint64, error) {
	providerCode := strings.TrimSpace(cfg.ProviderCode)
	if providerCode == "" {
		return 0, nil
	}
	if providerCode == "." || providerCode == ".." || filepath.Base(providerCode) != providerCode {
		return 0, fmt.Errorf("invalid request archive provider code %q", providerCode)
	}
	packagedRoot := strings.TrimSpace(cfg.PackagedRoot)
	if packagedRoot == "" {
		packagedRoot = defaultPackagedArchiveRoot
	}
	manifestPath := filepath.Join(packagedRoot, providerCode, dateDir, "batch_manifest.json")
	file, err := os.Open(manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open request archive batch manifest: %w", err)
	}
	defer file.Close()

	var manifest batchManifest
	if err := json.NewDecoder(file).Decode(&manifest); err != nil {
		return 0, fmt.Errorf("decode request archive batch manifest %q: %w", manifestPath, err)
	}
	if manifest.Date != dateDir || manifest.ProviderCode != providerCode {
		return 0, fmt.Errorf("request archive batch manifest identity mismatch: %s", manifestPath)
	}
	prefix := "req_" + dateID + "_"
	var highest uint64
	for _, batch := range manifest.Batches {
		if !strings.HasPrefix(batch.LastRequest, prefix) {
			return 0, fmt.Errorf("invalid last_request %q in %s", batch.LastRequest, manifestPath)
		}
		sequence, err := strconv.ParseUint(strings.TrimPrefix(batch.LastRequest, prefix), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid last_request %q in %s: %w", batch.LastRequest, manifestPath, err)
		}
		if sequence > highest {
			highest = sequence
		}
	}
	return highest, nil
}

func validateArchiveFilesystem(stagingParent, finalParent string) error {
	stageDir, err := os.MkdirTemp(stagingParent, ".archive-startup-probe-")
	if err != nil {
		return fmt.Errorf("request archive directory is not writable: %w", err)
	}
	defer os.RemoveAll(stageDir)
	finalDir := filepath.Join(finalParent, filepath.Base(stageDir))
	defer os.RemoveAll(finalDir)
	if err := os.Rename(stageDir, finalDir); err != nil {
		return fmt.Errorf("request archive directory does not support atomic publish: %w", err)
	}
	return nil
}

// FailureCount exposes recorded archive failures for operational metrics.
func FailureCount(reason string) uint64 {
	if reason == FailureReasonSequenceConflict {
		return sequenceConflictFailures.Load()
	}
	return 0
}

type Metadata struct {
	RequestID    string  `json:"requestId"`
	SessionID    *string `json:"sessionId"`
	RequestTime  string  `json:"requestTime"`
	HTTPStatus   int     `json:"httpStatus"`
	Protocol     string  `json:"protocol"`
	RequestMode  string  `json:"requestMode"`
	ModelName    *string `json:"modelName"`
	Platform     *string `json:"platform"`
	InputTokens  *int64  `json:"inputTokens"`
	OutputTokens *int64  `json:"outputTokens"`
	ProviderCode *string `json:"providerCode"`
	Error        string  `json:"error,omitempty"`
}

type Info struct {
	Protocol, Model, Platform string
	Stream                    bool
	Status                    int
}
type scopeKey struct{}

// Scope 由同一入站请求的串行重试和故障切换尝试共享。
// 只有当前 Scope 独占创建的目录才允许更新。
type Scope struct {
	mu                  sync.Mutex
	cfg                 Config
	id, session         string
	started             time.Time
	dir, finalDir       string
	current             *capture
	finished, published bool
}

func WithScope(ctx context.Context, cfg Config, session string, started time.Time) (context.Context, *Scope) {
	s := &Scope{cfg: cfg, session: session, started: started}
	return context.WithValue(ctx, scopeKey{}, s), s
}
func FromContext(ctx context.Context) *Scope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(scopeKey{}).(*Scope)
	return s
}
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Wrap 只在收到上游响应后调用。磁盘写入失败不得修改上游响应，
// 也不得中断计费或向客户端返回数据。
func (s *Scope) Wrap(body io.ReadCloser, request []byte, info Info, eventStream bool, contentLength int64) io.ReadCloser {
	if s == nil || body == nil {
		return body
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return body
	}
	if s.current != nil {
		s.current.finish(false, nil)
		if err := clearArchiveAttempt(s.dir); err != nil {
			s.report(err)
			return body
		}
		s.current = nil
	}
	if s.dir == "" {
		id, dir, finalDir, err := reserveArchiveDirectory(s.cfg.Root, s.started)
		if err != nil {
			s.report(err)
			return body
		}
		s.id, s.dir, s.finalDir = id, dir, finalDir
	}
	mode, name := "SYNC", "biz_response.json"
	if info.Stream {
		mode, name = "STREAM", "biz_stream_response.jsonl"
	}
	meta := Metadata{RequestID: s.id, SessionID: optional(s.session), RequestTime: s.started.In(archiveLocation).Format("2006-01-02 15:04:05"), HTTPStatus: info.Status, Protocol: info.Protocol, RequestMode: mode, ModelName: optional(info.Model), Platform: optional(info.Platform), ProviderCode: optional(s.cfg.ProviderCode)}
	c := &capture{dir: s.dir, meta: meta, sse: eventStream, expected: contentLength}
	s.current = c
	// 当前归档策略不做脱敏，直接保存实际上游请求的原始 JSON。
	// 脱敏实现保留在 privacy.go，待策略启用后再接入此处。
	if err := writeAtomic(s.dir, "biz_request.json", request); err != nil {
		c.fail(err)
	}
	file, err := os.OpenFile(filepath.Join(s.dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		c.fail(err)
	} else {
		c.file = file
		c.responseName = name
	}
	return &bodyReader{ReadCloser: body, c: c}
}
func (s *Scope) Finish() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	if s.current != nil {
		s.current.finish(false, nil)
		if s.current.ready() {
			if err := validateArchiveFiles(s.dir, s.current.responseName); err != nil {
				s.current.fail(err)
			} else if err := os.Rename(s.dir, s.finalDir); err != nil {
				s.current.fail(err)
			} else {
				s.current.setDirectory(s.finalDir)
				s.published = true
			}
		}
	}
	if !s.published {
		s.cleanupStaging()
	}
}

// SetUsage 写入网关最终计量得到的总 Token 数。当流式上游未在 SSE 中提供 usage
// 时，该值具有最终权威性。它可能在响应体 EOF 后执行，此时只会重写 metadata.json。
func (s *Scope) SetUsage(inputTokens, outputTokens int64) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil {
		s.current.setUsage(inputTokens, outputTokens)
	}
}
func (s *Scope) report(err error) {
	slog.Error("request archive failed", "request_id", s.id, "error", err)
}

// reserveArchiveDirectory 为归档独立分配全天唯一编号，并先在隐藏临时区中占位。
// 正式日期目录只在三个文件全部写入完成后出现。
func reserveArchiveDirectory(root string, started time.Time) (string, string, string, error) {
	root = filepath.Clean(strings.TrimSpace(root))
	localTime := started.In(archiveLocation)
	dateDir := localTime.Format("2006-01-02")
	dateID := localTime.Format("20060102")
	prefix := "req_" + dateID + "_"
	finalParent := filepath.Join(root, dateDir)
	stagingParent := filepath.Join(root, ".staging", dateDir)

	archiveAllocators.RLock()
	allocator := archiveAllocators.values[root]
	archiveAllocators.RUnlock()
	if allocator == nil {
		return "", "", "", errors.New("request archive is not initialized")
	}
	id, err := allocator.nextID(dateID, prefix, finalParent, stagingParent)
	if err != nil {
		return "", "", "", err
	}

	stageDir := filepath.Join(stagingParent, id)
	finalDir := filepath.Join(finalParent, id)
	if err := os.Mkdir(stageDir, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			sequenceConflictFailures.Add(1)
			return "", "", "", fmt.Errorf("request archive sequence conflict for %s: %w", id, err)
		}
		return "", "", "", err
	}
	return id, stageDir, finalDir, nil
}

func (a *sequenceAllocator) nextID(dateID, prefix, finalParent, stagingParent string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if dateID < a.date {
		return "", fmt.Errorf("request archive date moved backwards from %s to %s", a.date, dateID)
	}
	if dateID > a.date {
		if err := os.MkdirAll(finalParent, 0700); err != nil {
			return "", err
		}
		if err := os.MkdirAll(stagingParent, 0700); err != nil {
			return "", err
		}
		a.date = dateID
		a.sequence = 0
	}
	a.sequence++
	return fmt.Sprintf("%s%06d", prefix, a.sequence), nil
}

func highestArchiveSequence(prefix string, parents ...string) (uint64, error) {
	var highest uint64
	for _, parent := range parents {
		entries, err := os.ReadDir(parent)
		if err != nil {
			return 0, fmt.Errorf("scan request archive directory %q: %w", parent, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
				continue
			}
			sequence, err := strconv.ParseUint(strings.TrimPrefix(entry.Name(), prefix), 10, 64)
			if err == nil && sequence > highest {
				highest = sequence
			}
		}
	}
	return highest, nil
}

func clearArchiveAttempt(dir string) error {
	for _, name := range []string{"metadata.json", "biz_request.json", "biz_response.json", "biz_stream_response.jsonl"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func validateArchiveFiles(dir, responseName string) error {
	want := map[string]struct{}{
		"metadata.json":    {},
		"biz_request.json": {},
		responseName:       {},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != len(want) {
		return errors.New("archive does not contain exactly three files")
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return errors.New("archive contains an unexpected directory")
		}
		if _, ok := want[entry.Name()]; !ok {
			return fmt.Errorf("archive contains unexpected file %q", entry.Name())
		}
	}
	return nil
}

func (s *Scope) cleanupStaging() {
	if s.dir == "" {
		return
	}
	if err := os.RemoveAll(s.dir); err != nil {
		s.report(err)
	}
}
func writeAtomic(dir, name string, data []byte) error {
	f, err := os.CreateTemp(dir, ".archive-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

type bodyReader struct {
	io.ReadCloser
	c *capture
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.c.accept(p[:n])
	if err != nil {
		b.c.finish(err == io.EOF, err)
	}
	return n, err
}
func (b *bodyReader) Close() error {
	err := b.ReadCloser.Close()
	b.c.finish(false, err)
	return err
}

type capture struct {
	mu                    sync.Mutex
	dir, responseName     string
	meta                  Metadata
	file                  *os.File
	sse, closed, terminal bool
	complete, failed      bool
	expected, received    int64
	line, event           []byte
	hasData               bool
	invalid               bool
	authoritativeUsage    bool
	usage                 usageTotals
	rawResponse           []byte
}

func (c *capture) fail(err error) {
	c.failed = true
	if c.meta.Error == "" {
		// 当前归档策略不做脱敏，错误信息也按原样记录。
		c.meta.Error = err.Error()
		slog.Error("request archive failed", "request_id", c.meta.RequestID, "error", c.meta.Error)
	}
}

func (c *capture) ready() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed && c.complete && !c.failed && c.responseName != ""
}

func (c *capture) setDirectory(dir string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dir = dir
}
func (c *capture) writeMetadata() error {
	data, err := json.MarshalIndent(c.meta, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(c.dir, "metadata.json", data)
}
func (c *capture) write(data []byte) {
	if c.file == nil {
		return
	}
	if _, err := c.file.Write(data); err != nil {
		c.fail(err)
	}
}
func (c *capture) accept(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.received += int64(len(data))
	if !c.sse {
		c.rawResponse = append(c.rawResponse, data...)
		return
	}
	c.acceptSSE(data)
}

func (c *capture) setUsage(inputTokens, outputTokens int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	input := inputTokens
	output := outputTokens
	c.meta.InputTokens = &input
	c.meta.OutputTokens = &output
	c.authoritativeUsage = true
	if c.closed {
		if err := c.writeMetadata(); err != nil {
			c.fail(err)
		}
	}
}
func (c *capture) finish(eof bool, readErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if readErr != nil && readErr != io.EOF {
		c.fail(readErr)
	}
	complete := eof || c.expected >= 0 && c.received == c.expected
	if c.sse {
		// SSE 到达 EOF 时会派发尚未以空行结束的事件，即使上游遗漏了最后的空行。
		// 客户端主动关闭不具备这个保证，因此仍视为未完整读取。
		if eof {
			if len(c.line) > 0 {
				line := c.line
				if line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				c.sseLine(line)
				c.line = c.line[:0]
			}
			c.flushEvent()
		} else if len(c.line) > 0 || c.hasData {
			c.invalid = true
		}
		complete = (complete || c.terminal) && !c.invalid
	}
	if !c.sse && c.responseName != "" {
		raw := c.rawResponse
		if !json.Valid(raw) {
			complete = false
			c.fail(errors.New("incomplete or non-JSON upstream response"))
		} else {
			c.observeUsage(raw)
			// 当前归档策略不做脱敏，直接保存原始 JSON 响应。
			c.write(raw)
		}
	}
	if c.file != nil {
		if err := c.file.Close(); err != nil {
			c.fail(err)
		}
	}
	if !c.authoritativeUsage {
		c.meta.InputTokens, c.meta.OutputTokens = c.usage.totals(c.meta.Protocol)
	}
	if err := c.writeMetadata(); err != nil {
		c.fail(err)
	}
	c.complete = complete && !c.failed
	c.line = nil
	c.event = nil
	c.rawResponse = nil
}
