# jsonrepair

`jsonrepair` is a Go library and CLI for repairing common malformed JSON output, especially JSON-like text produced by LLMs.

It can handle cases such as:

- Markdown code fences around JSON
- Single quotes
- Unquoted object keys or string values
- Missing closing brackets or braces
- Trailing commas
- Unescaped quotes inside string values
- Full-width JSON punctuation often seen in Chinese text

## Install

```bash
go get github.com/kiry163/jsonrepair
```

## Use as a Go Library

Use `RepairJSON` when you want an error returned if repair fails.

```go
package main

import (
	"fmt"

	"github.com/kiry163/jsonrepair"
)

func main() {
	input := `{"name": "John is "good",hah", "age": 30}`

	output, err := jsonrepair.RepairJSON(input)
	if err != nil {
		panic(err)
	}

	fmt.Println(output)
}
```

Output:

```json
{"name":"John is \"good\",hah","age":30}
```

Use `MustRepairJSON` when you prefer an empty string on unexpected parser failure.

```go
package main

import (
	"fmt"

	"github.com/kiry163/jsonrepair"
)

func main() {
	input := "{'key': 'value', trailing: true,}"
	output := jsonrepair.MustRepairJSON(input)

	fmt.Println(output)
}
```

Output:

```json
{"key":"value","trailing":true}
```

## Repair Into a Go Value

Use `RepairInto` when the expected Go type is known. The target type is used as
a structural hint while repairing malformed JSON, which helps distinguish
string content from object keys and constrains nested value types.

```go
type Response struct {
	Answer  string `json:"answer"`
	Success bool   `json:"success"`
}

input := `{"answer":"foo", "unknown": 30, bar","success":true}`

var response Response
report, err := jsonrepair.RepairInto(input, &response)
if err != nil {
	panic(err)
}

fmt.Println(response.Answer)
fmt.Println(report.Strategy)
```

`RepairInto` writes to the target only after repair, schema validation, and
decoding all succeed. On failure, the target is left unchanged. The report
contains the selected repaired JSON, strategy, warnings, and semantic repair
actions.

Strict mode rejects unknown fields, missing required fields, unresolved
ambiguity, and schema mismatches:

```go
report, err := jsonrepair.RepairInto(
	input,
	&response,
	jsonrepair.WithStrict(),
	jsonrepair.WithRequiredFields("answer", "success"),
)
```

Unknown-field behavior and conservative string-to-number or string-to-boolean
conversion can be configured independently:

```go
report, err := jsonrepair.RepairInto(
	input,
	&response,
	jsonrepair.WithUnknownFields(jsonrepair.UnknownWarn),
	jsonrepair.WithTypeCoercion(true),
)
```

## Chinese Text Example

`jsonrepair` can preserve unescaped quotation marks inside Chinese string values.

```go
package main

import (
	"fmt"

	"github.com/kiry163/jsonrepair"
)

func main() {
	input := `{"result":"摘要包含"目的""方法""结果""结论"四要素。","passed":false}`

	output, err := jsonrepair.RepairJSON(input)
	if err != nil {
		panic(err)
	}

	fmt.Println(output)
}
```

Output:

```json
{"result":"摘要包含\"目的\"\"方法\"\"结果\"\"结论\"四要素。","passed":false}
```

Chinese object keys are also valid:

```go
input := `{"姓名":"张三","结论":"通过"}`
output, _ := jsonrepair.RepairJSON(input)
fmt.Println(output)
```

Output:

```json
{"姓名":"张三","结论":"通过"}
```

## Use as a CLI

Build the CLI:

```bash
go build -o jsonrepair ./cli
```

Repair inline input:

```bash
./jsonrepair -i '{"name": "John is "good",hah", "age": 30}'
```

Output:

```json
{"name":"John is \"good\",hah","age":30}
```

Repair a file:

```bash
./jsonrepair -f ./broken.json
```

Show help:

```bash
./jsonrepair -h
```

Show version:

```bash
./jsonrepair -v
```

Inject a version when building:

```bash
go build -ldflags "-X main.version=0.1.0" -o jsonrepair ./cli
```

## API

```go
func RepairJSON(src string) (dst string, err error)
```

Repairs JSON-like input and returns compact valid JSON when possible.

```go
func MustRepairJSON(src string) string
```

Repairs JSON-like input and returns an empty string if the parser panics.

## Notes

- Output is compact JSON.
- Object key order may change after repair because Go maps do not preserve key order.
- The library is designed for practical repair of malformed JSON-like text, not for strict validation.
