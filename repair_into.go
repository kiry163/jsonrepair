package jsonrepair

import (
	"encoding"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Strategy identifies the path that produced the selected result.
type Strategy string

const (
	StrategyDirect  Strategy = "direct"
	StrategyGeneric Strategy = "generic"
	StrategyGuided  Strategy = "guided"
)

type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

type IssueCode string

const (
	IssueUnknownField      IssueCode = "unknown_field"
	IssueMissingField      IssueCode = "missing_field"
	IssueAmbiguousBoundary IssueCode = "ambiguous_boundary"
	IssueTypeMismatch      IssueCode = "type_mismatch"
	IssueTypeCoercion      IssueCode = "type_coercion"
	IssueDataLoss          IssueCode = "data_loss"
	IssueLimitExceeded     IssueCode = "limit_exceeded"
)

type RepairIssue struct {
	Severity Severity
	Code     IssueCode
	Path     string
	Message  string
}

type ActionKind string

const (
	ActionCloseString  ActionKind = "close_string"
	ActionCloseObject  ActionKind = "close_object"
	ActionCloseArray   ActionKind = "close_array"
	ActionRecoverField ActionKind = "recover_field"
	ActionInsertComma  ActionKind = "insert_comma"
	ActionCoerceType   ActionKind = "coerce_type"
)

type RepairAction struct {
	Kind   ActionKind
	Path   string
	Detail string
}

type RepairReport struct {
	RepairedJSON string
	Strategy     Strategy
	Issues       []RepairIssue
	Actions      []RepairAction
}

type Mode int

const (
	ModeBestEffort Mode = iota
	ModeStrict
)

type UnknownFieldPolicy int

const (
	UnknownWarn UnknownFieldPolicy = iota
	UnknownAllow
	UnknownReject
)

type repairOptions struct {
	mode           Mode
	unknownFields  UnknownFieldPolicy
	unknownSet     bool
	requiredFields []string
	typeCoercion   bool
	maxCandidates  int
	maxDepth       int
}

type Option func(*repairOptions)

func WithMode(mode Mode) Option {
	return func(opts *repairOptions) { opts.mode = mode }
}

func WithStrict() Option {
	return WithMode(ModeStrict)
}

func WithRequiredFields(paths ...string) Option {
	return func(opts *repairOptions) {
		for _, path := range paths {
			path = normalizeRequiredPath(path)
			if path != "" {
				opts.requiredFields = append(opts.requiredFields, path)
			}
		}
	}
}

func WithUnknownFields(policy UnknownFieldPolicy) Option {
	return func(opts *repairOptions) {
		opts.unknownFields = policy
		opts.unknownSet = true
	}
}

func WithTypeCoercion(enabled bool) Option {
	return func(opts *repairOptions) { opts.typeCoercion = enabled }
}

func WithMaxCandidates(n int) Option {
	return func(opts *repairOptions) { opts.maxCandidates = n }
}

func WithMaxDepth(n int) Option {
	return func(opts *repairOptions) { opts.maxDepth = n }
}

type ErrorCode string

const (
	ErrorInvalidTarget  ErrorCode = "invalid_target"
	ErrorInvalidOption  ErrorCode = "invalid_option"
	ErrorUnrepairable   ErrorCode = "unrepairable"
	ErrorSchemaMismatch ErrorCode = "schema_mismatch"
	ErrorAmbiguous      ErrorCode = "ambiguous"
	ErrorLimitExceeded  ErrorCode = "limit_exceeded"
)

type RepairError struct {
	Code    ErrorCode
	Path    string
	Message string
	Cause   error
}

func (e *RepairError) Error() string {
	if e == nil {
		return ""
	}
	if e.Path == "" {
		return fmt.Sprintf("jsonrepair: %s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("jsonrepair: %s at %s: %s", e.Code, e.Path, e.Message)
}

func (e *RepairError) Unwrap() error { return e.Cause }

// RepairInto repairs src using the type of dst as a structural hint, then
// atomically decodes the selected result into dst. dst must be a non-nil
// pointer. Existing RepairJSON behavior is preserved for callers that do not
// have a target type.
func RepairInto(src string, dst any, options ...Option) (*RepairReport, error) {
	opts := repairOptions{
		mode:          ModeBestEffort,
		unknownFields: UnknownWarn,
		maxCandidates: 32,
		maxDepth:      128,
	}
	for _, option := range options {
		if option != nil {
			option(&opts)
		}
	}
	if opts.mode == ModeStrict && !opts.unknownSet {
		opts.unknownFields = UnknownReject
	}

	target := reflect.ValueOf(dst)
	if !target.IsValid() || target.Kind() != reflect.Pointer || target.IsNil() {
		return nil, &RepairError{
			Code:    ErrorInvalidTarget,
			Message: "dst must be a non-nil pointer",
		}
	}
	if opts.mode != ModeBestEffort && opts.mode != ModeStrict {
		return nil, &RepairError{Code: ErrorInvalidOption, Message: "invalid repair mode"}
	}
	if opts.unknownFields < UnknownWarn || opts.unknownFields > UnknownReject {
		return nil, &RepairError{Code: ErrorInvalidOption, Message: "invalid unknown field policy"}
	}
	if opts.maxCandidates <= 0 || opts.maxDepth <= 0 {
		return nil, &RepairError{
			Code:    ErrorInvalidOption,
			Message: "max candidates and max depth must be positive",
		}
	}

	meta := buildTypeMeta(target.Elem().Type(), map[reflect.Type]bool{})
	var candidates []repairCandidate

	// Valid JSON is authoritative. We validate it against the target, but do
	// not reinterpret a valid structure just because it has unknown fields.
	if json.Valid([]byte(src)) {
		candidates = append(candidates, repairCandidate{json: src, strategy: StrategyDirect})
	} else {
		candidate, err := RepairJSON(src)
		if err != nil {
			return &RepairReport{Strategy: StrategyGeneric}, &RepairError{Code: ErrorUnrepairable, Message: "could not repair input", Cause: err}
		}
		candidates = append(candidates, repairCandidate{json: candidate, strategy: StrategyGeneric})
		if len(candidates) < opts.maxCandidates {
			guided, ok := guidedRepairJSON(src, meta, opts)
			if ok {
				candidates = append(candidates, repairCandidate{json: guided, strategy: StrategyGuided})
			}
		}
	}

	var selected *evaluatedCandidate
	for _, candidate := range candidates {
		evaluated, err := evaluateCandidate(candidate, meta, opts)
		if err != nil {
			continue
		}
		if selected == nil || candidateBetter(evaluated, *selected) {
			selected = &evaluated
		}
	}
	if selected == nil {
		return &RepairReport{}, &RepairError{Code: ErrorUnrepairable, Message: "no repair candidate produced valid JSON"}
	}

	report := &selected.report
	if hasErrorIssue(report.Issues) {
		return report, &RepairError{
			Code:    ErrorSchemaMismatch,
			Message: "repaired value does not satisfy target schema",
		}
	}

	temporary := reflect.New(target.Elem().Type())
	if err := json.Unmarshal([]byte(report.RepairedJSON), temporary.Interface()); err != nil {
		return report, &RepairError{Code: ErrorSchemaMismatch, Message: "could not decode repaired value into dst", Cause: err}
	}
	target.Elem().Set(temporary.Elem())
	return report, nil
}

type repairCandidate struct {
	json     string
	strategy Strategy
}

type evaluatedCandidate struct {
	report RepairReport
	score  candidateScore
}

type candidateScore struct {
	knownFields   int
	arrayElements int
	stringBytes   int
}

func evaluateCandidate(candidate repairCandidate, meta *typeMeta, opts repairOptions) (evaluatedCandidate, error) {
	report := RepairReport{Strategy: candidate.strategy}
	if candidate.strategy == StrategyGuided {
		severity := SeverityWarning
		if opts.mode == ModeStrict {
			severity = SeverityError
		}
		report.Issues = append(report.Issues, RepairIssue{
			Severity: severity,
			Code:     IssueAmbiguousBoundary,
			Path:     "$",
			Message:  "schema guidance was required to resolve a structural boundary",
		})
		report.Actions = append(report.Actions, RepairAction{
			Kind:   ActionRecoverField,
			Path:   "$",
			Detail: "selected schema-guided repair candidate",
		})
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(candidate.json))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return evaluatedCandidate{}, err
	}

	if opts.typeCoercion {
		var actions []RepairAction
		value, actions = coerceValue(value, meta, "$")
		report.Actions = append(report.Actions, actions...)
		for _, action := range actions {
			report.Issues = append(report.Issues, RepairIssue{
				Severity: SeverityWarning,
				Code:     IssueTypeCoercion,
				Path:     action.Path,
				Message:  action.Detail,
			})
		}
	}

	validateValue(value, meta, "$", &report.Issues, opts)
	for _, required := range opts.requiredFields {
		if !hasRequiredPath(value, required) {
			severity := SeverityWarning
			if opts.mode == ModeStrict {
				severity = SeverityError
			}
			report.Issues = append(report.Issues, RepairIssue{
				Severity: severity,
				Code:     IssueMissingField,
				Path:     "$." + required,
				Message:  "required field is missing",
			})
		}
	}
	score := measureCandidate(value, meta)

	encoded, err := JSONMarshal(value)
	if err != nil {
		return evaluatedCandidate{}, err
	}
	report.RepairedJSON = strings.TrimSpace(string(encoded))
	return evaluatedCandidate{report: report, score: score}, nil
}

func candidateBetter(left, right evaluatedCandidate) bool {
	leftErrors, leftUnknown, leftWarnings := candidateIssueCounts(left.report.Issues)
	rightErrors, rightUnknown, rightWarnings := candidateIssueCounts(right.report.Issues)
	if leftErrors != rightErrors {
		return leftErrors < rightErrors
	}
	if leftUnknown != rightUnknown {
		return leftUnknown < rightUnknown
	}
	if left.score.knownFields != right.score.knownFields {
		return left.score.knownFields > right.score.knownFields
	}
	if left.score.arrayElements != right.score.arrayElements {
		return left.score.arrayElements > right.score.arrayElements
	}
	if left.score.stringBytes != right.score.stringBytes {
		return left.score.stringBytes > right.score.stringBytes
	}
	if leftWarnings != rightWarnings {
		return leftWarnings < rightWarnings
	}
	// Keep the generic parser on a tie; it has the broadest regression suite.
	return left.report.Strategy == StrategyGeneric && right.report.Strategy != StrategyGeneric
}

func measureCandidate(value any, meta *typeMeta) candidateScore {
	var score candidateScore
	if meta == nil || meta.opaque {
		return score
	}
	switch typed := value.(type) {
	case map[string]any:
		if meta.kind == reflect.Struct {
			for key, child := range typed {
				field, ok := lookupSchemaField(meta, key)
				if !ok {
					continue
				}
				score.knownFields++
				childScore := measureCandidate(child, field.meta)
				score = addCandidateScore(score, childScore)
			}
		} else if meta.kind == reflect.Map {
			for _, child := range typed {
				score = addCandidateScore(score, measureCandidate(child, meta.value))
			}
		}
	case []any:
		if meta.kind == reflect.Array || meta.kind == reflect.Slice {
			score.arrayElements += len(typed)
			for _, child := range typed {
				score = addCandidateScore(score, measureCandidate(child, meta.item))
			}
		}
	case string:
		score.stringBytes += len(typed)
	}
	return score
}

func addCandidateScore(left, right candidateScore) candidateScore {
	left.knownFields += right.knownFields
	left.arrayElements += right.arrayElements
	left.stringBytes += right.stringBytes
	return left
}

func candidateIssueCounts(issues []RepairIssue) (errors, unknown, warnings int) {
	for _, issue := range issues {
		if issue.Severity == SeverityError {
			errors++
		} else {
			warnings++
		}
		if issue.Code == IssueUnknownField {
			unknown++
		}
	}
	return errors, unknown, warnings
}

func hasErrorIssue(issues []RepairIssue) bool {
	for _, issue := range issues {
		if issue.Severity == SeverityError {
			return true
		}
	}
	return false
}

type typeMeta struct {
	kind        reflect.Kind
	typ         reflect.Type
	nullable    bool
	opaque      bool
	stringValue bool
	fields      map[string]fieldMeta
	item        *typeMeta
	value       *typeMeta
}

type fieldMeta struct {
	name  string
	meta  *typeMeta
	index []int
}

func buildTypeMeta(typ reflect.Type, seen map[reflect.Type]bool) *typeMeta {
	if typ.Kind() == reflect.Pointer {
		meta := buildTypeMeta(typ.Elem(), seen)
		meta.nullable = true
		return meta
	}
	meta := &typeMeta{kind: typ.Kind(), typ: typ}
	textUnmarshaler := reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	jsonUnmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	if typ == reflect.TypeOf(json.RawMessage{}) ||
		typ.Implements(jsonUnmarshaler) ||
		reflect.PointerTo(typ).Implements(jsonUnmarshaler) ||
		typ.Implements(textUnmarshaler) ||
		reflect.PointerTo(typ).Implements(textUnmarshaler) {
		meta.opaque = true
		return meta
	}
	if seen[typ] {
		meta.kind = reflect.Invalid
		return meta
	}
	seen[typ] = true
	defer delete(seen, typ)

	switch typ.Kind() {
	case reflect.Struct:
		meta.fields = make(map[string]fieldMeta)
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if field.PkgPath != "" && !field.Anonymous {
				continue
			}
			name, stringValue, skip := jsonFieldName(field)
			if skip {
				continue
			}
			if field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct {
				nested := buildTypeMeta(field.Type, seen)
				for nestedName, nestedField := range nested.fields {
					nestedField.index = append([]int{i}, nestedField.index...)
					meta.fields[nestedName] = nestedField
				}
				continue
			}
			if name == "" {
				name = field.Name
			}
			meta.fields[name] = fieldMeta{
				name:  name,
				meta:  buildTypeMeta(field.Type, seen),
				index: []int{i},
			}
			if stringValue {
				fieldMeta := meta.fields[name]
				fieldMeta.meta.stringValue = true
				meta.fields[name] = fieldMeta
			}
		}
	case reflect.Array, reflect.Slice:
		meta.item = buildTypeMeta(typ.Elem(), seen)
	case reflect.Map:
		meta.value = buildTypeMeta(typ.Elem(), seen)
	}
	return meta
}

func jsonFieldName(field reflect.StructField) (string, bool, bool) {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return "", false, false
	}
	parts := strings.Split(tag, ",")
	if parts[0] == "-" {
		return "", false, true
	}
	return parts[0], containsString(parts[1:], "string"), false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func validateValue(value any, meta *typeMeta, path string, issues *[]RepairIssue, opts repairOptions) {
	if meta == nil || meta.opaque || meta.kind == reflect.Invalid || meta.kind == reflect.Interface {
		return
	}
	if value == nil {
		if meta.nullable {
			return
		}
		switch meta.kind {
		case reflect.Map, reflect.Slice, reflect.Interface:
			return
		default:
			appendTypeIssue(issues, path, meta, "null is not valid for this field")
			return
		}
	}

	switch typed := value.(type) {
	case map[string]any:
		if meta.kind != reflect.Struct && meta.kind != reflect.Map {
			appendTypeIssue(issues, path, meta, "object value has incompatible target type")
			return
		}
		if meta.kind == reflect.Map {
			for key, child := range typed {
				validateValue(child, meta.value, path+"."+key, issues, opts)
			}
			return
		}
		for key, child := range typed {
			field, ok := meta.fields[key]
			if !ok {
				for knownName, knownField := range meta.fields {
					if strings.EqualFold(knownName, key) {
						field, ok = knownField, true
						break
					}
				}
			}
			if !ok {
				if opts.unknownFields != UnknownAllow {
					severity := SeverityWarning
					if opts.unknownFields == UnknownReject {
						severity = SeverityError
					}
					*issues = append(*issues, RepairIssue{Severity: severity, Code: IssueUnknownField, Path: path + "." + key, Message: "field is not present in target schema"})
				}
				continue
			}
			validateValue(child, field.meta, path+"."+key, issues, opts)
		}
	case []any:
		if meta.kind != reflect.Array && meta.kind != reflect.Slice {
			appendTypeIssue(issues, path, meta, "array value has incompatible target type")
			return
		}
		for i, child := range typed {
			validateValue(child, meta.item, fmt.Sprintf("%s[%d]", path, i), issues, opts)
		}
	case json.Number:
		if !isNumericKind(meta.kind) {
			appendTypeIssue(issues, path, meta, "number value has incompatible target type")
		}
	case string:
		if meta.kind != reflect.String && !(meta.stringValue && (isNumericKind(meta.kind) || meta.kind == reflect.Bool)) {
			appendTypeIssue(issues, path, meta, "string value has incompatible target type")
		}
	case bool:
		if meta.kind != reflect.Bool && !(meta.stringValue && meta.kind == reflect.Bool) {
			appendTypeIssue(issues, path, meta, "boolean value has incompatible target type")
		}
	}
}

func appendTypeIssue(issues *[]RepairIssue, path string, meta *typeMeta, message string) {
	*issues = append(*issues, RepairIssue{Severity: SeverityError, Code: IssueTypeMismatch, Path: path, Message: message})
}

func isNumericKind(kind reflect.Kind) bool {
	return kind >= reflect.Int && kind <= reflect.Float64 || kind >= reflect.Uint && kind <= reflect.Uintptr
}

func coerceValue(value any, meta *typeMeta, path string) (any, []RepairAction) {
	var actions []RepairAction
	if meta == nil {
		return value, actions
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if meta.kind == reflect.Struct {
				if field, ok := meta.fields[key]; ok {
					var childActions []RepairAction
					typed[key], childActions = coerceValue(child, field.meta, path+"."+key)
					actions = append(actions, childActions...)
				}
			} else if meta.kind == reflect.Map {
				var childActions []RepairAction
				typed[key], childActions = coerceValue(child, meta.value, path+"."+key)
				actions = append(actions, childActions...)
			}
		}
	case []any:
		if meta.kind == reflect.Array || meta.kind == reflect.Slice {
			for i, child := range typed {
				var childActions []RepairAction
				typed[i], childActions = coerceValue(child, meta.item, fmt.Sprintf("%s[%d]", path, i))
				actions = append(actions, childActions...)
			}
		}
	case string:
		if meta.stringValue {
			break
		}
		if isNumericKind(meta.kind) {
			if _, err := strconv.ParseFloat(typed, 64); err == nil {
				value = json.Number(typed)
				actions = append(actions, RepairAction{Kind: ActionCoerceType, Path: path, Detail: "string converted to number"})
			}
		} else if meta.kind == reflect.Bool {
			if _, err := strconv.ParseBool(typed); err == nil {
				value = strings.EqualFold(typed, "true")
				actions = append(actions, RepairAction{Kind: ActionCoerceType, Path: path, Detail: "string converted to boolean"})
			}
		}
	}
	return value, actions
}

func normalizeRequiredPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "$")
	path = strings.TrimPrefix(path, ".")
	return path
}

func hasRequiredPath(value any, path string) bool {
	path = normalizeRequiredPath(path)
	if path == "" {
		return true
	}
	parts := strings.Split(path, ".")
	current := value
	for _, part := range parts {
		if part == "" {
			continue
		}
		obj, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = obj[part]
		if !ok {
			return false
		}
	}
	return true
}
