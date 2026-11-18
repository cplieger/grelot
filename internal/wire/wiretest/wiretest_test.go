package wiretest_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cplieger/grelot/internal/wire/wiretest"
)

func loadFixture(t *testing.T) *wiretest.Doc {
	t.Helper()
	d, err := wiretest.Load(filepath.Join("testdata", "openapi.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return d
}

func jsonHeader() http.Header {
	return http.Header{"Content-Type": {"application/json; charset=utf-8"}}
}

const validItem = `{"id":1,"kind":"plain","name":"abc","slug":"ab","at":"2026-11-15T10:00:00.5Z","score":3,` +
	`"tags":["x"],"labels":{"a":1},"note":null,"ok":true}`

// TestCheck_acceptsAConformingAnswer pins the passing side, so each refusal
// below is the one changed member and not a fixture the validator always refuses.
func TestCheck_acceptsAConformingAnswer(t *testing.T) {
	d := loadFixture(t)
	cases := []struct {
		name, method, path string
		status             int
		header             http.Header
		body               string
		op                 string
	}{
		{"object", "GET", "/v1/items/7", 200, jsonHeader(), validItem, "getItem"},
		{"required only", "GET", "/v1/items/7", 200, jsonHeader(), `{"id":0,"kind":"fancy"}`, "getItem"},
		{"null array", "GET", "/v1/items/7", 200, jsonHeader(), `{"id":0,"kind":"fancy","tags":null}`, "getItem"},
		{"string note", "GET", "/v1/items/7", 200, jsonHeader(), `{"id":0,"kind":"fancy","note":"n"}`, "getItem"},
		{"list", "GET", "/v1/items", 200, jsonHeader(), `{"items":[` + validItem + `],"next_cursor":"c","total":2}`, "listItems"},
		{"empty list", "GET", "/v1/items?limit=5", 200, jsonHeader(), `{"items":[],"total":0}`, "listItems"},
		{"declared code", "GET", "/v1/items", 429, jsonHeader(), `{"error":"slow down","code":"rate_limited","request_id":"r"}`, "listItems"},
		{"literal beats parameter", "GET", "/v1/items/mine", 200, jsonHeader(), `{"items":[],"total":0}`, "listMyItems"},
		{"escaped segment", "GET", "/v1/items/a%2Fb", 200, jsonHeader(), `{"id":0,"kind":"plain"}`, "getItem"},
		{"no content", "DELETE", "/v1/items/7", 204, http.Header{}, ``, "deleteItem"},
		{
			"location header", "POST", "/v1/items", 201,
			http.Header{"Content-Type": {"application/json"}, "Location": {"/v1/items/1"}},
			`{"id":1,"kind":"plain"}`, "createItem",
		},
		{"one union case", "GET", "/v1/shapes", 200, jsonHeader(), `{"shape":"circle","r":2}`, "getShape"},
		{"stream", "GET", "/v1/events", 200, http.Header{"Content-Type": {"text/event-stream"}}, "event: sse:hello\ndata: {}\n\n", "events"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op, err := d.Check(c.method, c.path, c.status, c.header, []byte(c.body))
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if op != c.op {
				t.Errorf("operation = %q, want %q", op, c.op)
			}
		})
	}
}

func TestCheck_refusesANonConformingAnswer(t *testing.T) {
	d := loadFixture(t)
	item := func(old, repl string) string { return strings.Replace(validItem, old, repl, 1) }
	cases := []struct {
		name, method, path string
		status             int
		header             http.Header
		body               string
		want               string
	}{
		{"missing required field", "GET", "/v1/items/7", 200, jsonHeader(), `{"id":1}`, "kind"},
		{"extra field", "GET", "/v1/items/7", 200, jsonHeader(), item(`"ok":true`, `"ok":true,"extra":1`), "extra"},
		{"extra field in a list item", "GET", "/v1/items", 200, jsonHeader(), `{"items":[{"id":1,"kind":"plain","x":1}],"total":1}`, "x"},
		{"wrong enum", "GET", "/v1/items/7", 200, jsonHeader(), item(`"plain"`, `"loud"`), "loud"},
		{"bad date-time", "GET", "/v1/items/7", 200, jsonHeader(), item(`"2026-11-15T10:00:00.5Z"`, `"2026-11-15 10:00"`), "date-time"},
		{"date-time without zone", "GET", "/v1/items/7", 200, jsonHeader(), item(`"2026-11-15T10:00:00.5Z"`, `"2026-11-15T10:00:00"`), "date-time"},
		{"undeclared status", "GET", "/v1/items/7", 404, jsonHeader(), `{"error":"gone","code":"not_found"}`, "404"},
		{"unknown path", "GET", "/v1/nothing", 200, jsonHeader(), `{}`, "/v1/nothing"},
		{"unknown method", "PUT", "/v1/items", 200, jsonHeader(), `{}`, "PUT"},
		{"too many segments", "GET", "/v1/items/7/x", 200, jsonHeader(), `{}`, "/v1/items/7/x"},
		{"code outside the enum", "GET", "/v1/items", 429, jsonHeader(), `{"error":"slow","code":"busy"}`, "busy"},
		{"declared codes but no code", "GET", "/v1/items", 429, jsonHeader(), `{"error":"slow"}`, "code"},
		{"wrong content type", "GET", "/v1/items/7", 200, http.Header{"Content-Type": {"text/plain"}}, validItem, "text/plain"},
		{"missing content type", "GET", "/v1/items/7", 200, http.Header{}, validItem, "Content-Type"},
		{"json where a stream is declared", "GET", "/v1/events", 200, jsonHeader(), `{}`, "application/json"},
		{"body where none is declared", "DELETE", "/v1/items/7", 204, http.Header{}, `{}`, "body"},
		{"missing required header", "POST", "/v1/items", 201, jsonHeader(), `{"id":1,"kind":"plain"}`, "Location"},
		{"not json", "GET", "/v1/items/7", 200, jsonHeader(), `{"id":1,`, "JSON"},
		{"trailing data", "GET", "/v1/items/7", 200, jsonHeader(), `{"id":1,"kind":"plain"} {}`, "JSON"},
		{"wrong type", "GET", "/v1/items/7", 200, jsonHeader(), item(`"name":"abc"`, `"name":3`), "string"},
		{"fraction for integer", "GET", "/v1/items/7", 200, jsonHeader(), item(`"id":1`, `"id":1.5`), "integer"},
		{"below minimum", "GET", "/v1/items/7", 200, jsonHeader(), item(`"score":3`, `"score":0`), "minimum"},
		{"above maximum", "GET", "/v1/items/7", 200, jsonHeader(), item(`"score":3`, `"score":6`), "maximum"},
		{"below minLength", "GET", "/v1/items/7", 200, jsonHeader(), item(`"name":"abc"`, `"name":""`), "minLength"},
		{"above maxLength in code points", "GET", "/v1/items/7", 200, jsonHeader(), item(`"name":"abc"`, `"name":"abcd"`), "maxLength"},
		{"above x-max-bytes", "GET", "/v1/items/7", 200, jsonHeader(), item(`"name":"abc"`, `"name":"éé€"`), "x-max-bytes"},
		{"pattern", "GET", "/v1/items/7", 200, jsonHeader(), item(`"slug":"ab"`, `"slug":"A1"`), "pattern"},
		{"below minItems", "GET", "/v1/items/7", 200, jsonHeader(), item(`"tags":["x"]`, `"tags":[]`), "minItems"},
		{"above maxItems", "GET", "/v1/items/7", 200, jsonHeader(), item(`"tags":["x"]`, `"tags":["x","y","z"]`), "maxItems"},
		{"null where none is allowed", "GET", "/v1/items/7", 200, jsonHeader(), item(`"ok":true`, `"ok":null`), "boolean"},
		{"map value type", "GET", "/v1/items/7", 200, jsonHeader(), item(`"labels":{"a":1}`, `"labels":{"a":"1"}`), "integer"},
		{"map key name", "GET", "/v1/items/7", 200, jsonHeader(), item(`"labels":{"a":1}`, `"labels":{"A":1}`), "A"},
		{"anyOf none matches", "GET", "/v1/items/7", 200, jsonHeader(), item(`"note":null`, `"note":4`), "anyOf"},
		{"oneOf no case", "GET", "/v1/shapes", 200, jsonHeader(), `{"shape":"triangle","r":2}`, "oneOf"},
		{"oneOf case with a foreign field", "GET", "/v1/shapes", 200, jsonHeader(), `{"shape":"circle","r":2,"side":1}`, "oneOf"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := d.Check(c.method, c.path, c.status, c.header, []byte(c.body))
			if err == nil {
				t.Fatal("Check accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name %q", err, c.want)
			}
		})
	}
}

func TestOperations_listsEveryOperationOnce(t *testing.T) {
	got := strings.Join(loadFixture(t).Operations(), ",")
	want := "createItem,deleteItem,events,getItem,getShape,listItems,listMyItems"
	if got != want {
		t.Errorf("Operations() = %s, want %s", got, want)
	}
}

// TestLoad_refusesAKeywordItCannotCheck keeps the validator honest against
// the generator: a schema keyword it would silently skip fails the load.
func TestLoad_refusesAKeywordItCannotCheck(t *testing.T) {
	cases := map[string]string{
		"unknown keyword":  `{"type":"object","properties":{"a":{"type":"string","multipleOf":2}}}`,
		"unresolvable ref": `{"$ref":"#/components/schemas/Missing"}`,
		"bad pattern":      `{"type":"string","pattern":"("}`,
	}
	for name, schema := range cases {
		t.Run(name, func(t *testing.T) {
			doc := `{"openapi":"3.1.0","paths":{"/a":{"get":{"operationId":"a","responses":{"200":{"description":"OK",` +
				`"content":{"application/json":{"schema":{"$ref":"#/components/schemas/A"}}}}}}}},` +
				`"components":{"schemas":{"A":` + schema + `}}}`
			path := filepath.Join(t.TempDir(), "openapi.json")
			if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := wiretest.Load(path); err == nil {
				t.Fatal("Load accepted it")
			}
		})
	}
}

// TestLoad_readsTheShippedDocument keeps every keyword the generator emits
// into docs/openapi.json within what Check validates.
func TestLoad_readsTheShippedDocument(t *testing.T) {
	d, err := wiretest.Load(filepath.Join("..", "..", "..", "docs", "openapi.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(d.Operations()) == 0 {
		t.Fatal("no operations")
	}
}
