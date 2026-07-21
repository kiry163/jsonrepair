package jsonrepair

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// guidedRepairJSON is intentionally isolated from JSONParser. It produces an
// additional candidate using target field names and field types as boundary
// hints; the generic parser remains available and wins ties.
func guidedRepairJSON(src string, meta *typeMeta, opts repairOptions) (string, bool) {
	p := &guidedParser{
		src:      normalizeGuidedInput(src),
		maxDepth: opts.maxDepth,
	}
	if meta != nil && meta.kind == reflect.Struct {
		if start := strings.IndexAny(p.src, "{["); start > 0 {
			p.index = start
		}
	}
	value, ok := p.parseValue(meta, nil, false, 0)
	if !ok {
		return "", false
	}
	encoded, err := JSONMarshal(value)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(encoded)), true
}

// normalizeGuidedInput removes transport wrappers and comments without
// rewriting punctuation inside string values. The generic repair path keeps
// its historical normalizeInput behavior; schema-guided parsing must preserve
// the content it is trying to recover.
func normalizeGuidedInput(src string) string {
	src = stripCodeFences(src)
	src = stripComments(src)
	return src
}

type guidedParser struct {
	src      string
	index    int
	maxDepth int
}

func (p *guidedParser) parseValue(meta, parent *typeMeta, inArray bool, depth int) (any, bool) {
	if depth > p.maxDepth {
		return nil, false
	}
	if meta != nil && meta.opaque {
		meta = anyTypeMeta()
	}
	p.skipSpace()
	if p.index >= len(p.src) {
		return nil, false
	}

	switch p.src[p.index] {
	case '{':
		return p.parseObject(meta, depth+1)
	case '[':
		return p.parseArray(meta, depth+1)
	case '"', '\'':
		return p.parseQuotedString(meta, parent, inArray)
	default:
		return p.parseBareValue(meta, parent, inArray)
	}
}

func (p *guidedParser) parseObject(meta *typeMeta, depth int) (any, bool) {
	if depth > p.maxDepth || p.index >= len(p.src) || p.src[p.index] != '{' {
		return nil, false
	}
	p.index++
	result := make(map[string]any)

	for p.index < len(p.src) {
		p.skipSpaceAndCommas()
		if p.index >= len(p.src) {
			return result, true
		}
		if p.src[p.index] == '}' {
			p.index++
			return result, true
		}

		start := p.index
		key, ok := p.parseObjectKey()
		if !ok || key == "" {
			p.index = start + 1
			continue
		}

		fieldType := anyTypeMeta()
		if meta != nil && meta.kind == reflect.Struct {
			if field, found := lookupSchemaField(meta, key); found {
				fieldType = field.meta
			}
		}

		valueStart := p.index
		value, ok := p.parseValue(fieldType, meta, false, depth)
		if !ok {
			// Preserve the key and allow the candidate evaluator to report a
			// schema mismatch rather than looping on malformed input.
			result[key] = ""
			if p.index <= valueStart {
				p.index++
			}
		} else {
			result[key] = value
		}
	}
	return result, true
}

func (p *guidedParser) parseArray(meta *typeMeta, depth int) (any, bool) {
	if depth > p.maxDepth || p.index >= len(p.src) || p.src[p.index] != '[' {
		return nil, false
	}
	p.index++
	result := make([]any, 0)
	itemType := anyTypeMeta()
	if meta != nil && (meta.kind == reflect.Array || meta.kind == reflect.Slice) && meta.item != nil {
		itemType = meta.item
	}

	for p.index < len(p.src) {
		p.skipSpaceAndCommas()
		if p.index >= len(p.src) {
			return result, true
		}
		if p.src[p.index] == ']' {
			p.index++
			return result, true
		}
		// A common LLM error closes an array with an extra object brace.
		if p.src[p.index] == '}' {
			return result, true
		}

		start := p.index
		value, ok := p.parseValue(itemType, meta, true, depth)
		if !ok {
			if p.index <= start {
				p.index++
			}
			continue
		}
		result = append(result, value)
	}
	return result, true
}

func (p *guidedParser) parseObjectKey() (string, bool) {
	p.skipSpace()
	if p.index >= len(p.src) {
		return "", false
	}

	if p.src[p.index] == '"' || p.src[p.index] == '\'' {
		delim := p.src[p.index]
		p.index++
		start := p.index
		for p.index < len(p.src) {
			if p.src[p.index] == '\\' {
				p.index += 2
				continue
			}
			if p.src[p.index] == delim {
				key := p.src[start:p.index]
				p.index++
				p.skipSpace()
				if p.index < len(p.src) && p.src[p.index] == ':' {
					p.index++
					return key, true
				}
			}
			p.index++
		}
		return "", false
	}

	start := p.index
	for p.index < len(p.src) {
		c := p.src[p.index]
		if c == ':' || unicode.IsSpace(rune(c)) {
			break
		}
		if c == ',' || c == '}' || c == ']' {
			return "", false
		}
		p.index++
	}
	key := strings.TrimSpace(p.src[start:p.index])
	p.skipSpace()
	if p.index < len(p.src) && p.src[p.index] == ':' {
		p.index++
		return key, key != ""
	}
	return "", false
}

func (p *guidedParser) parseQuotedString(meta, parent *typeMeta, inArray bool) (any, bool) {
	delim := p.src[p.index]
	p.index++
	var value strings.Builder

	for p.index < len(p.src) {
		c := p.src[p.index]
		if c == '\\' && p.index+1 < len(p.src) {
			next := p.src[p.index+1]
			switch next {
			case 'n':
				value.WriteByte('\n')
			case 'r':
				value.WriteByte('\r')
			case 't':
				value.WriteByte('\t')
			case 'b':
				value.WriteByte('\b')
			case 'f':
				value.WriteByte('\f')
			case 'u':
				if p.index+6 <= len(p.src) {
					code, err := strconv.ParseUint(p.src[p.index+2:p.index+6], 16, 16)
					if err == nil {
						r := rune(code)
						if utf16.IsSurrogate(r) && p.index+12 <= len(p.src) && p.src[p.index+6:p.index+8] == "\\u" {
							low, lowErr := strconv.ParseUint(p.src[p.index+8:p.index+12], 16, 16)
							if lowErr == nil {
								r = utf16.DecodeRune(r, rune(low))
								p.index += 6
							}
						}
						if r != unicode.ReplacementChar {
							var encoded [utf8.UTFMax]byte
							n := utf8.EncodeRune(encoded[:], r)
							value.Write(encoded[:n])
							p.index += 6
							continue
						}
					}
				}
				value.WriteByte(next)
			default:
				value.WriteByte(next)
			}
			p.index += 2
			continue
		}
		if c == delim {
			if p.isGuidedStringClose(p.index+1, parent, inArray) {
				p.index++
				return value.String(), true
			}
			value.WriteByte(c)
			p.index++
			continue
		}
		// Infer a missing final quote only when the brace/bracket also has a
		// plausible structural lookahead. Braces in formulas and prose are
		// otherwise ordinary string content (for example, "\\arg{max}").
		if meta != nil && meta.kind == reflect.String && p.isImplicitStringClose(p.index, inArray) {
			return strings.TrimRightFunc(value.String(), unicode.IsSpace), true
		}
		value.WriteByte(c)
		p.index++
	}
	return strings.TrimRightFunc(value.String(), unicode.IsSpace), true
}

func (p *guidedParser) isImplicitStringClose(index int, inArray bool) bool {
	if index >= len(p.src) {
		return false
	}
	c := p.src[index]
	if (!inArray && c != '}') || (inArray && c != ']') {
		return false
	}
	next := p.skipSpaceAt(index + 1)
	if next >= len(p.src) {
		return true
	}
	switch p.src[next] {
	case ',', '}', ']':
		return true
	default:
		return false
	}
}

func (p *guidedParser) isGuidedStringClose(after int, parent *typeMeta, inArray bool) bool {
	i := p.skipSpaceAt(after)
	if i >= len(p.src) {
		return true
	}
	switch p.src[i] {
	case '}':
		return !inArray
	case ']':
		return inArray
	case ',':
		if inArray {
			return true
		}
		key, ok := p.peekObjectKey(p.skipSpaceAt(i + 1))
		if !ok {
			return false
		}
		_, known := lookupSchemaField(parent, key)
		return known
	default:
		return false
	}
}

func (p *guidedParser) peekObjectKey(index int) (string, bool) {
	if index >= len(p.src) {
		return "", false
	}
	if p.src[index] == '"' || p.src[index] == '\'' {
		delim := p.src[index]
		start := index + 1
		for i := start; i < len(p.src); i++ {
			if p.src[i] == '\\' {
				i++
				continue
			}
			if p.src[i] == delim {
				next := p.skipSpaceAt(i + 1)
				return p.src[start:i], next < len(p.src) && p.src[next] == ':'
			}
		}
		return "", false
	}
	start := index
	for index < len(p.src) && !unicode.IsSpace(rune(p.src[index])) && p.src[index] != ':' {
		if p.src[index] == ',' || p.src[index] == '}' || p.src[index] == ']' {
			return "", false
		}
		index++
	}
	next := p.skipSpaceAt(index)
	return p.src[start:index], next < len(p.src) && p.src[next] == ':'
}

func (p *guidedParser) parseBareValue(meta, parent *typeMeta, inArray bool) (any, bool) {
	start := p.index
	for p.index < len(p.src) {
		c := p.src[p.index]
		if c == '}' || c == ']' {
			break
		}
		if c == ',' {
			if inArray {
				break
			}
			key, ok := p.peekObjectKey(p.skipSpaceAt(p.index + 1))
			if ok {
				if _, known := lookupSchemaField(parent, key); known {
					break
				}
			}
		}
		p.index++
	}
	token := strings.TrimSpace(p.src[start:p.index])
	if token == "" {
		return nil, false
	}

	if meta != nil && meta.kind == reflect.String {
		return token, true
	}
	switch strings.ToLower(token) {
	case "true":
		return true, true
	case "false":
		return false, true
	case "null":
		return nil, true
	}
	if _, err := strconv.ParseFloat(token, 64); err == nil {
		return json.Number(token), true
	}
	return token, true
}

func (p *guidedParser) skipSpace() {
	for p.index < len(p.src) && unicode.IsSpace(rune(p.src[p.index])) {
		p.index++
	}
}

func (p *guidedParser) skipSpaceAndCommas() {
	for p.index < len(p.src) {
		if unicode.IsSpace(rune(p.src[p.index])) || p.src[p.index] == ',' {
			p.index++
			continue
		}
		break
	}
}

func (p *guidedParser) skipSpaceAt(index int) int {
	for index < len(p.src) && unicode.IsSpace(rune(p.src[index])) {
		index++
	}
	return index
}

func lookupSchemaField(meta *typeMeta, name string) (fieldMeta, bool) {
	if meta == nil || meta.fields == nil {
		return fieldMeta{}, false
	}
	if field, ok := meta.fields[name]; ok {
		return field, true
	}
	for known, field := range meta.fields {
		if strings.EqualFold(known, name) {
			return field, true
		}
	}
	return fieldMeta{}, false
}

func anyTypeMeta() *typeMeta {
	return &typeMeta{kind: reflect.Interface}
}
