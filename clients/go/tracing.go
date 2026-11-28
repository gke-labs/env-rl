// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package env

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/metadata"
)

// DefaultTraceServiceName prefixes span names when
// ClientOptions.TraceServiceName is empty.
const DefaultTraceServiceName = "ate-env-go-client"

// Span attribute keys, the vocabulary shared with the Python client:
// Substrate identities use Substrate's ateattr keys (ate.*), so spans join
// Substrate's own logs and traces; client-only fields use ate_env.*.
// Values carry ids, the executable name and sizes; never a command line or
// file content.
const (
	AttrActorName         = attribute.Key("ate.actor.name")
	AttrAtespace          = attribute.Key("ate.atespace")
	AttrTemplateName      = attribute.Key("ate.template.name")
	AttrTemplateAtespace  = attribute.Key("ate.template.atespace")
	AttrRootfsImage       = attribute.Key("ate_env.rootfs_image")
	AttrCommandExecutable = attribute.Key("ate_env.command.executable")
	AttrFirstCommand      = attribute.Key("ate_env.first_command")
	AttrExitCode          = attribute.Key("ate_env.exit_code")
	AttrTimedOut          = attribute.Key("ate_env.timed_out")
	AttrFilePath          = attribute.Key("ate_env.file.path")
	AttrFileSize          = attribute.Key("ate_env.file.size")
)

// Operations, the part of each span name after "<TraceServiceName>.".
const (
	opCreateEnvironment        = "create_environment"
	opGetEnvironment           = "get_environment"
	opListEnvironments         = "list_environments"
	opSuspendEnvironment       = "suspend_environment"
	opDeleteEnvironment        = "delete_environment"
	opSetEnvironmentTimeToLive = "set_environment_time_to_live"
	opForkEnvironment          = "fork_environment"
	opStartProcess             = "start_process"
	opExec                     = "exec"
	opReadFile                 = "read_file"
	opWriteFile                = "write_file"
	opGetServerInfo            = "get_server_info"
	opGetImageStatus           = "get_image_status"
	opEnsureImageTemplate      = "ensure_image_template"
	opListPlacementDomains     = "list_placement_domains"
	opWarmImage                = "warm_image"
)

// tracePropagator is the only propagator the client uses: W3C Trace
// Context, no Baggage.
var tracePropagator propagation.TextMapPropagator = propagation.TraceContext{}

// NewTracerProvider returns a TracerProvider that batches spans to an
// OTLP/gRPC exporter, for callers who want the OTLP default. The exporter
// reads OTEL_EXPORTER_OTLP_ENDPOINT (default localhost:4317) and
// OTEL_EXPORTER_OTLP_INSECURE; serviceName becomes the service.name resource
// attribute. Pass it as ClientOptions.TracerProvider. The caller owns it and
// must call Shutdown to flush.
func NewTracerProvider(ctx context.Context, serviceName string) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("env: creating OTLP trace exporter: %w", err)
	}
	res, err := sdkresource.Merge(sdkresource.Default(),
		sdkresource.NewSchemaless(attribute.String("service.name", serviceName)))
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return nil, fmt.Errorf("env: building OTel resource: %w", err)
	}
	return sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res)), nil
}

// clientTracer starts the client's spans. The zero value (no provider)
// starts none and leaves ctx untouched.
type clientTracer struct {
	tracer trace.Tracer
	svc    string
	// inject writes the span's W3C context into the outgoing metadata
	// itself, for a caller-supplied GRPCConn that has no otelgrpc handler.
	inject bool
	// target is the caller-supplied GRPCConn's target, for server.address
	// and server.port on its CLIENT spans.
	target string
}

func newClientTracer(opts ClientOptions) clientTracer {
	if opts.TracerProvider == nil {
		return clientTracer{}
	}
	svc := opts.TraceServiceName
	if svc == "" {
		svc = DefaultTraceServiceName
	}
	t := clientTracer{
		tracer: opts.TracerProvider.Tracer(strings.ReplaceAll(svc, "-", "_")),
		svc:    svc,
		inject: opts.GRPCConn != nil,
	}
	if opts.GRPCConn != nil {
		t.target = opts.GRPCConn.Target()
	}
	return t
}

// noopSpan is the non-recording span every call returns without a provider.
var noopSpan = trace.SpanFromContext(context.Background())

// environmentService is the gRPC service of the spans startRPC opens.
const environmentService = "ateenv.v1alpha.EnvironmentService"

// start opens the INTERNAL span "<svc>.<op>" as a child of the span in ctx,
// for an operation that may make several RPCs (exec makes StartProcess and
// StreamProcessOutput). Empty attribute values are not recorded.
func (t clientTracer) start(ctx context.Context, op string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return t.open(ctx, op, trace.SpanKindInternal, attrs)
}

// startRPC opens the span of an operation that makes exactly one
// EnvironmentService RPC, method. On the client's own connection, the
// otelgrpc handler adds the RPC's CLIENT span (with rpc.* attributes) under
// this one, so this span is INTERNAL: two nested CLIENT spans would make
// trace backends draw the client-to-server edge twice. On a caller-supplied
// GRPCConn (t.inject) there is no otelgrpc span, so this span is the RPC's
// CLIENT span and carries the rpc.* keys of rpcAttrs, server.address and
// server.port, as the Python client's spans do.
func (t clientTracer) startRPC(ctx context.Context, op, method string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if !t.inject {
		return t.open(ctx, op, trace.SpanKindInternal, attrs)
	}
	return t.open(ctx, op, trace.SpanKindClient, append(attrs, rpcAttrs(environmentService, method, t.target)...))
}

func (t clientTracer) open(ctx context.Context, op string, kind trace.SpanKind, attrs []attribute.KeyValue) (context.Context, trace.Span) {
	if t.tracer == nil {
		return ctx, noopSpan
	}
	ctx, span := t.tracer.Start(ctx, t.svc+"."+op, trace.WithSpanKind(kind), trace.WithAttributes(nonEmpty(attrs)...))
	if t.inject {
		carrier := propagation.MapCarrier{}
		tracePropagator.Inject(ctx, carrier)
		if len(carrier) > 0 {
			md, ok := metadata.FromOutgoingContext(ctx)
			if ok {
				md = md.Copy()
			} else {
				md = metadata.MD{}
			}
			for k, v := range carrier {
				md.Set(k, v)
			}
			ctx = metadata.NewOutgoingContext(ctx, md)
		}
	}
	return ctx, span
}

// setAttributes sets the attributes with a non-empty value on span.
func setAttributes(span trace.Span, attrs ...attribute.KeyValue) {
	if attrs = nonEmpty(attrs); len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
}

// nonEmpty drops string attributes whose value is empty, as the Python
// client drops None values: an unknown value is not recorded as "".
func nonEmpty(attrs []attribute.KeyValue) []attribute.KeyValue {
	out := attrs[:0:0]
	for _, kv := range attrs {
		if kv.Value.Type() == attribute.STRING && kv.Value.AsString() == "" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// The rpc.* convention rpcAttrs follows, as otelgrpc selects it.
const (
	semconvNew = "new" // rpc.system.name and the full rpc.method (default)
	semconvOld = "old" // rpc.system, rpc.service and the short rpc.method
	semconvDup = "dup" // both sets; rpc.method is the full name
)

// parseRPCSemconvMode reads OTEL_SEMCONV_STABILITY_OPT_IN as otelgrpc
// v0.72.0's parseSemconvMode does, and as the Python client's
// _rpc_semconv_mode does: a comma-separated list, each item trimmed; the
// first item that is rpc/dup or rpc/old decides, and anything else means new.
func parseRPCSemconvMode(value string) string {
	for item := range strings.SplitSeq(value, ",") {
		switch strings.TrimSpace(item) {
		case "rpc/dup":
			return semconvDup
		case "rpc/old":
			return semconvOld
		}
	}
	return semconvNew
}

// rpcSemconvMode is read once, as otelgrpc reads it once per handler.
var rpcSemconvMode = parseRPCSemconvMode(os.Getenv("OTEL_SEMCONV_STABILITY_OPT_IN"))

// rpcAttrs are the OTel semantic-convention attributes of one gRPC call,
// with the same keys and values as the Python client's rpc_attrs and as
// otelgrpc v0.72.0's client spans: by default rpc.system.name and
// rpc.method with the full "<service>/<method>"; with
// OTEL_SEMCONV_STABILITY_OPT_IN=rpc/old the older rpc.system, rpc.service
// and the short rpc.method; with rpc/dup both sets, with the full
// rpc.method, the value otelgrpc keeps. target is the connection's target:
// "host:port", optionally behind gRPC's dns:/// or passthrough:/// scheme;
// another form (such as unix:///path) is recorded whole as server.address.
func rpcAttrs(service, method, target string) []attribute.KeyValue {
	fullMethod := service + "/" + method
	var attrs []attribute.KeyValue
	switch rpcSemconvMode {
	case semconvOld:
		attrs = []attribute.KeyValue{
			attribute.String("rpc.system", "grpc"),
			attribute.String("rpc.service", service),
			attribute.String("rpc.method", method),
		}
	case semconvDup:
		attrs = []attribute.KeyValue{
			attribute.String("rpc.system", "grpc"),
			attribute.String("rpc.service", service),
			attribute.String("rpc.system.name", "grpc"),
			attribute.String("rpc.method", fullMethod),
		}
	default:
		attrs = []attribute.KeyValue{
			attribute.String("rpc.system.name", "grpc"),
			attribute.String("rpc.method", fullMethod),
		}
	}
	for _, scheme := range []string{"dns:///", "passthrough:///"} {
		target = strings.TrimPrefix(target, scheme)
	}
	if target == "" {
		return attrs
	}
	host, port := target, ""
	if i := strings.LastIndex(target, ":"); i > 0 {
		host, port = target[:i], target[i+1:]
	}
	if n, err := strconv.Atoi(port); err == nil && host != "" {
		return append(attrs, attribute.String("server.address", strings.Trim(host, "[]")), attribute.Int("server.port", n))
	}
	return append(attrs, attribute.String("server.address", target))
}

// imageRefAttr is an image reference without its tag, for span attributes,
// as the Python client's image_ref_attr: "repo@sha256:..." (the digest is an
// id) and "repo" are kept; "repo:tag" and "repo:tag@sha256:..." drop the
// tag.
func imageRefAttr(ref string) string {
	name, digest, hasDigest := strings.Cut(ref, "@")
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name = name[:colon]
	}
	if hasDigest {
		return name + "@" + digest
	}
	return name
}

// endSpan records err on span, if any, and ends it.
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// envAssignment matches a leading shell variable assignment, NAME=value or
// bash's NAME+=value, as in "API_KEY=s3cr3t python x.py". It is the same
// pattern as the Python client's _ENV_ASSIGNMENT.
var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)

// ops are the shell control and redirection operators the tokenizer splits
// out of a word, longest match first; redirBases are the redirection ones,
// which may follow an fd number ("2>", "3>&"). "((" starts an arithmetic
// command, as in bash's lexer; "( (" is two subshells.
var (
	ops = []string{"\n", "(", "((", ")", ";", ";;", "&", "&&", "|", "||",
		"<", ">", ">>", "<<", "<<<", ">&", "<&", "&>", "&>>", "<>", ">|", "<<-"}
	redirBases = []string{"<", ">", ">>", "<<", "<<<", ">&", "<&", "<>", ">|", "<<-"}
)

// stripASCIIDigits removes a leading ASCII fd number ("2>&" -> ">&").
func stripASCIIDigits(op string) string {
	i := 0
	for i < len(op) && op[i] >= '0' && op[i] <= '9' {
		i++
	}
	return op[i:]
}

// isOpPrefix reports whether op starts an operator; shellWords uses it to find the longest operator.
func isOpPrefix(op string) bool {
	base := stripASCIIDigits(op)
	if len(base) < len(op) {
		if len(base) == 0 {
			return true
		}
		for _, r := range redirBases {
			if strings.HasPrefix(r, base) {
				return true
			}
		}
		return false
	}
	for _, o := range ops {
		if strings.HasPrefix(o, base) {
			return true
		}
	}
	return false
}

// isRedirectionOp reports whether op is a shell redirection operator, with an optional ASCII fd number.
func isRedirectionOp(op string) bool {
	base := stripASCIIDigits(op)
	for _, r := range redirBases {
		if r == base {
			return true
		}
	}
	return op == "&>" || op == "&>>"
}

// maxExecutableLen is the longest executable name recorded, in bytes: a file
// name longer than NAME_MAX (255 on Linux) cannot be a command, and the cap
// bounds the attribute.
const maxExecutableLen = 255

// shellToken is a word or an operator from shellWords. quoted is the byte
// offset in text of the first character that was quoted or escaped, or -1. A
// quoted character is never syntax: "'('" is a word, "\"A=b\"" is not an
// assignment and a quoted "2" before ">" is not an fd number.
type shellToken struct {
	text   string
	op     bool
	quoted int
}

// executable returns the executable name of a shell command line, as the
// Python client's executable_attr does: the base name of the first command
// word of the line's first simple command, never an argument. The line is
// split like a POSIX shell splits it (quotes, backslashes, comments), so
// "'my prog' arg" gives "my prog" and "cmd # A=s" gives "cmd". Before the
// command word it skips:
//   - leading NAME=value and NAME+=value assignments whose name and "=" are
//     unquoted, which can carry secrets;
//   - redirections and their targets ("2>/dev/null", ">|f", "3>&-",
//     "2>&-cmd", "<<<input"), with an optional unquoted ASCII fd number;
//   - unquoted opening "(", "{" and newlines.
//
// Only the first simple command counts: a control operator (";", "&",
// "|", "&&", "||", ";;", ")") or an arithmetic command ("((x+1))") before
// the command word gives "", so
// "cmd1 && A=s cmd2" gives "cmd1" and "; A=s cmd" gives "". It also gives
// "" when an assignment's value or a redirection target holds "$" or a
// backtick (an expansion such as "$(echo my secret)" can span words, and
// its rest would read as the command), when a redirection has no target or
// its target is an operator ("2>&1<<<x", "A=>(x) cmd"), when the target of
// "<&" or ">&" starts with a quoted "-" ("2>&'-'cmd"), when the command word
// holds "=", "$" or a backtick, when the command word is longer than one
// character and holds "{" or "[" (a brace expansion such as
// "{mysql,-pSEKRET}" or "mysql{,-pSEKRET}" expands into arguments, and
// "A[1 + 2]=x cmd" is an array assignment), when the name is longer than
// maxExecutableLen, and when the line cannot be split (an unterminated
// quote, a trailing backslash). "" records no attribute. A reserved word
// that starts the line is recorded as the command word: "! cmd x" gives
// "!", "time cmd x" gives "time" and "if cmd; then x; fi" gives "if"
// ("[[ -f x ]]" gives no attribute because "[[" holds "[").
func executable(commandLine string) string {
	tokens, ok := shellWords(commandLine)
	if !ok {
		return ""
	}
	return executableWordsFromShell(tokens)
}

// executableWordsFromShell returns the executable name of the first simple command in tokens, or "".
func executableWordsFromShell(tokens []shellToken) string {
	inCommand := false
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		w := tok.text
		if tok.op {
			switch {
			case w == "\n" || w == "(":
				if inCommand {
					return ""
				}
			case isRedirectionOp(w):
				if i+1 >= len(tokens) {
					return ""
				}
				target := tokens[i+1]
				if target.op || strings.ContainsAny(target.text, "$`") {
					return ""
				}
				// shellWords splits an unquoted "-" off after "<&" and
				// ">&", so a longer target starting with "-" had it quoted
				// ("2>&'-'cmd"): bash rejects the redirection and runs
				// nothing.
				if base := stripASCIIDigits(w); (base == "<&" || base == ">&") &&
					len(target.text) > 1 && target.text[0] == '-' {
					return ""
				}
				inCommand = true
				i++ // skip target
			default: // ";", "&", "|", "&&", "||", ";;", ")"
				return ""
			}
			continue
		}
		if w == "{" && tok.quoted < 0 {
			if inCommand {
				return ""
			}
			continue
		}
		if loc := envAssignment.FindStringIndex(w); loc != nil && (tok.quoted < 0 || tok.quoted >= loc[1]) {
			// An expansion in the value can span words, so the next
			// word may be the value's rest, not the command.
			if strings.ContainsAny(w, "$`") {
				return ""
			}
			inCommand = true
			continue
		}
		// A brace expansion expands into the command and its arguments, and
		// a "[" in a longer word may be an array subscript with spaces.
		if len(w) > 1 && strings.ContainsAny(w, "{[") {
			return ""
		}
		return commandName(w)
	}
	return ""
}

// commandName returns the base name of the command word w, or "" when w
// holds "=", "$" or a backtick or the name is longer than maxExecutableLen.
func commandName(w string) string {
	if strings.ContainsAny(w, "=$`") {
		return ""
	}
	name := w[strings.LastIndex(w, "/")+1:]
	if len(name) > maxExecutableLen {
		return ""
	}
	return name
}

// executableWords returns the base name of words[0], or "" when words is
// empty, words[0] holds "=", "$" or a backtick (so a leading assignment
// item records nothing) or the name is longer than maxExecutableLen,
// matching executable_attr on a sequence in Python.
func executableWords(words []string) string {
	if len(words) == 0 {
		return ""
	}
	return commandName(words[0])
}

// isASCIIDigits reports whether s is a non-empty run of ASCII digits.
func isASCIIDigits(s string) bool {
	return s != "" && stripASCIIDigits(s) == ""
}

// shellWords splits s into shell words and operator tokens, matching the
// Python client's _tokenize_sh: spaces and tabs separate words (a carriage
// return is part of a word, as in bash); unquoted control and redirection
// operators (ops, with an optional leading unquoted ASCII fd number on
// redirections, plus "-" as its own token after "<&" or ">&") become their
// own tokens; '#' outside a word starts a comment to the next newline; single
// quotes keep everything literal; inside double quotes a backslash escapes
// '"', '\\', '$', '`' and '\n'; outside quotes a backslash escapes the next
// character (joining lines on "\\\n"). Each word records where its first
// quoted or escaped character is. ok is false for an unterminated quote or a
// trailing backslash.
func shellWords(s string) (tokens []shellToken, ok bool) {
	var cur strings.Builder
	inWord := false
	quoted := -1
	flush := func() {
		if inWord {
			tokens = append(tokens, shellToken{text: cur.String(), quoted: quoted})
			cur.Reset()
			inWord = false
			quoted = -1
		}
	}
	markQuoted := func() {
		inWord = true
		if quoted < 0 {
			quoted = cur.Len()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case ' ', '\t':
			flush()
		case '\n', '(', ')', ';', '&', '|', '<', '>':
			var opStr string
			if (c == '<' || c == '>') && inWord && quoted < 0 && isASCIIDigits(cur.String()) {
				opStr = cur.String() + string(c)
				cur.Reset()
				inWord = false
			} else {
				flush()
				opStr = string(c)
			}

			// Bash removes a backslash-newline pair before it reads an operator,
			// so "<<", a line continuation and "-" are "<<-".
			for {
				j := i + 1
				for j+1 < len(s) && s[j] == '\\' && s[j+1] == '\n' {
					j += 2
				}
				if j >= len(s) || !isOpPrefix(opStr+string(s[j])) {
					break
				}
				opStr += string(s[j])
				i = j
			}
			tokens = append(tokens, shellToken{text: opStr, op: true, quoted: -1})
			if base := stripASCIIDigits(opStr); base == "<&" || base == ">&" {
				j := i + 1
				for j+1 < len(s) && s[j] == '\\' && s[j+1] == '\n' {
					j += 2
				}
				if j < len(s) && s[j] == '-' {
					tokens = append(tokens, shellToken{text: "-", quoted: -1})
					i = j
				}
			}
		case '\\':
			if i+1 >= len(s) {
				return nil, false
			}
			if s[i+1] == '\n' {
				i++
				continue
			}
			markQuoted()
			cur.WriteByte(s[i+1])
			i++
		case '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, false
			}
			markQuoted()
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
		case '"':
			markQuoted()
			closed := false
			for i++; i < len(s); i++ {
				if s[i] == '"' {
					closed = true
					break
				}
				if s[i] == '\\' && i+1 < len(s) {
					next := s[i+1]
					if next == '"' || next == '\\' || next == '$' || next == '\n' || next == '`' {
						if next != '\n' {
							cur.WriteByte(next)
						}
						i++
						continue
					}
				}
				cur.WriteByte(s[i])
			}
			if !closed {
				return nil, false
			}
		case '#':
			if !inWord {
				end := strings.IndexByte(s[i:], '\n')
				if end < 0 {
					i = len(s)
				} else {
					i += end - 1
				}
			} else {
				cur.WriteByte(c)
			}
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	flush()
	return tokens, true
}

// spanReader counts the bytes read through it and ends its span on EOF, on a
// read error or on Close, whichever comes first; ate_env.file.size is set
// only when the stream reaches io.EOF without an error, matching
// ate_env_rl.Env.read_file.
type spanReader struct {
	io.ReadCloser
	span trace.Span
	n    int64
	once sync.Once
}

func (r *spanReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.n += int64(n)
	if err == io.EOF {
		r.finish(nil, true)
	} else if err != nil {
		r.finish(err, false)
	}
	return n, err
}

func (r *spanReader) Close() error {
	err := r.ReadCloser.Close()
	r.finish(err, false)
	return err
}

func (r *spanReader) finish(err error, complete bool) {
	r.once.Do(func() {
		if complete && err == nil {
			r.span.SetAttributes(AttrFileSize.Int64(r.n))
		}
		endSpan(r.span, err)
	})
}
