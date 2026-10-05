package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/luwa07832/shop-order-service/internal/store"
)

func parseRFC3339(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	if parsed.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("timestamp %q is not UTC", value)
	}
	return parsed, nil
}

func newTestRouter(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, NewRouter(st)
}

func doJSON(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	} else {
		reader = bytes.NewReader(nil)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	return recorder
}

type orderResponse struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenant_id"`
	IdempotencyKey string `json:"idempotency_key"`
	Status         string `json:"status"`
	TotalAmount    int64  `json:"total_amount"`
	CreatedAt      string `json:"created_at"`
	Lines          []struct {
		ProductID string `json:"product_id"`
		Quantity  int64  `json:"quantity"`
		UnitPrice int64  `json:"unit_price"`
	} `json:"lines"`
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

const validOrder = `{
	"tenant_id":" tenant-a ",
	"idempotency_key":" key-1 ",
	"lines":[
		{"product_id":" p1 ","quantity":2,"unit_price":150},
		{"product_id":"p2","quantity":1,"unit_price":0}
	]
}`

func decodeOrder(t *testing.T, recorder *httptest.ResponseRecorder) orderResponse {
	t.Helper()
	var order orderResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &order); err != nil {
		t.Fatalf("decode order %q: %v", recorder.Body.String(), err)
	}
	return order
}

func TestCreateOrderSuccess(t *testing.T) {
	_, handler := newTestRouter(t)

	recorder := doJSON(handler, http.MethodPost, "/v1/orders", validOrder)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body %s", recorder.Code, http.StatusCreated, recorder.Body.String())
	}
	order := decodeOrder(t, recorder)
	if order.ID == "" {
		t.Fatal("id is empty")
	}
	if order.TenantID != " tenant-a " || order.IdempotencyKey != " key-1 " {
		t.Fatalf("identifiers not preserved verbatim: %q %q", order.TenantID, order.IdempotencyKey)
	}
	if order.Status != "created" {
		t.Fatalf("status = %q, want created", order.Status)
	}
	if order.TotalAmount != 300 {
		t.Fatalf("total_amount = %d, want 300", order.TotalAmount)
	}
	if len(order.Lines) != 2 || order.Lines[0].ProductID != " p1 " ||
		order.Lines[0].Quantity != 2 || order.Lines[0].UnitPrice != 150 ||
		order.Lines[1].ProductID != "p2" || order.Lines[1].Quantity != 1 ||
		order.Lines[1].UnitPrice != 0 {
		t.Fatalf("lines snapshot mismatch: %+v", order.Lines)
	}
	if _, err := parseRFC3339(order.CreatedAt); err != nil {
		t.Fatalf("created_at %q is not RFC3339 UTC: %v", order.CreatedAt, err)
	}
}

func TestCreateOrderReplay(t *testing.T) {
	_, handler := newTestRouter(t)

	first := doJSON(handler, http.MethodPost, "/v1/orders", validOrder)
	// Different JSON whitespace, reordered top-level/line fields, extra unknown
	// fields — semantically identical content.
	replayed := `{
		"extra":"ignored",
		"lines":[{"unit_price":150,"quantity":2,"product_id":" p1 ","note":"x"},
		         {"unit_price":0,"product_id":"p2","quantity":1}],
		"idempotency_key":" key-1 ",
		"tenant_id":" tenant-a "
	}`
	second := doJSON(handler, http.MethodPost, "/v1/orders", replayed)

	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}
	if second.Code != http.StatusOK {
		t.Fatalf("replay status = %d, want 200; body %s", second.Code, second.Body.String())
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("replay body differs:\nfirst:  %s\nsecond: %s", first.Body.String(), second.Body.String())
	}
}

func TestCreateOrderConflictAndOriginalUnchanged(t *testing.T) {
	_, handler := newTestRouter(t)

	first := doJSON(handler, http.MethodPost, "/v1/orders", validOrder)
	firstOrder := decodeOrder(t, first)

	// Different unit price on the same key.
	conflict := doJSON(handler, http.MethodPost, "/v1/orders", `{
		"tenant_id":" tenant-a ",
		"idempotency_key":" key-1 ",
		"lines":[{"product_id":" p1 ","quantity":2,"unit_price":151}]
	}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, want 409; body %s", conflict.Code, conflict.Body.String())
	}
	var envelope errorResponse
	if err := json.Unmarshal(conflict.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if envelope.Error.Code != "idempotency_conflict" || envelope.Error.Message == "" {
		t.Fatalf("unexpected error envelope: %+v", envelope.Error)
	}

	// Line order is part of the content: swapped lines conflict too.
	reordered := doJSON(handler, http.MethodPost, "/v1/orders", `{
		"tenant_id":" tenant-a ",
		"idempotency_key":" key-1 ",
		"lines":[{"product_id":"p2","quantity":1,"unit_price":0},
		         {"product_id":" p1 ","quantity":2,"unit_price":150}]
	}`)
	if reordered.Code != http.StatusConflict {
		t.Fatalf("reordered lines status = %d, want 409", reordered.Code)
	}

	// The original order is unchanged and still readable.
	fetched := doJSON(handler, http.MethodGet, "/v1/orders/"+firstOrder.ID+"?tenant_id=%20tenant-a%20", "")
	if fetched.Code != http.StatusOK || fetched.Body.String() != first.Body.String() {
		t.Fatalf("original order changed: status %d body %s", fetched.Code, fetched.Body.String())
	}
}

func TestIdempotencyKeyScopedByTenant(t *testing.T) {
	_, handler := newTestRouter(t)

	body := func(tenant string) string {
		return fmt.Sprintf(`{"tenant_id":%q,"idempotency_key":"same-key",
			"lines":[{"product_id":"p","quantity":1,"unit_price":1}]}`, tenant)
	}
	first := doJSON(handler, http.MethodPost, "/v1/orders", body("tenant-a"))
	second := doJSON(handler, http.MethodPost, "/v1/orders", body("tenant-b"))

	if first.Code != http.StatusCreated || second.Code != http.StatusCreated {
		t.Fatalf("statuses = %d, %d; want 201, 201", first.Code, second.Code)
	}
	o1, o2 := decodeOrder(t, first), decodeOrder(t, second)
	if o1.ID == o2.ID {
		t.Fatalf("tenants share order id %s", o1.ID)
	}
}

func TestConcurrentIdenticalSubmissions(t *testing.T) {
	_, handler := newTestRouter(t)

	const n = 20
	var wg sync.WaitGroup
	recorders := make([]*httptest.ResponseRecorder, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			recorders[i] = doJSON(handler, http.MethodPost, "/v1/orders", validOrder)
		}(i)
	}
	wg.Wait()

	var created, replays int
	var canonicalBody string
	for _, recorder := range recorders {
		switch recorder.Code {
		case http.StatusCreated:
			created++
			canonicalBody = recorder.Body.String()
		case http.StatusOK:
			replays++
		default:
			t.Fatalf("unexpected status %d body %s", recorder.Code, recorder.Body.String())
		}
	}
	if created != 1 || replays != n-1 {
		t.Fatalf("created = %d, replays = %d, want 1 and %d", created, replays, n-1)
	}
	for i, recorder := range recorders {
		if recorder.Body.String() != canonicalBody {
			t.Fatalf("response %d differs from canonical:\n%s\n%s", i, recorder.Body.String(), canonicalBody)
		}
	}
}

func TestConcurrentConflictingSubmissions(t *testing.T) {
	_, handler := newTestRouter(t)

	bodyA := `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"a","quantity":1,"unit_price":1}]}`
	bodyB := `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"b","quantity":1,"unit_price":1}]}`

	const perSide = 10
	recorders := make([]*httptest.ResponseRecorder, perSide*2)
	var wg sync.WaitGroup
	for i := 0; i < perSide*2; i++ {
		body := bodyA
		if i%2 == 0 {
			body = bodyB
		}
		wg.Add(1)
		go func(i int, body string) {
			defer wg.Done()
			recorders[i] = doJSON(handler, http.MethodPost, "/v1/orders", body)
		}(i, body)
	}
	wg.Wait()

	var createdCount, conflictCount, replayCount int
	var winner orderResponse
	for _, recorder := range recorders {
		switch recorder.Code {
		case http.StatusCreated:
			createdCount++
			winner = decodeOrder(t, recorder)
		case http.StatusOK:
			replayCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("unexpected status %d", recorder.Code)
		}
	}
	if createdCount != 1 {
		t.Fatalf("created = %d, want exactly 1", createdCount)
	}
	if conflictCount != perSide {
		t.Fatalf("conflicts = %d, want %d", conflictCount, perSide)
	}
	if replayCount != perSide-1 {
		t.Fatalf("replays = %d, want %d", replayCount, perSide-1)
	}

	// Every successful response describes the same order; the stored order is
	// the winner's snapshot.
	fetched := doJSON(handler, http.MethodGet, "/v1/orders/"+winner.ID+"?tenant_id=t", "")
	if fetched.Body.String() == "" {
		t.Fatal("winner order not queryable")
	}
	for _, recorder := range recorders {
		if recorder.Code == http.StatusOK && decodeOrder(t, recorder).ID != winner.ID {
			t.Fatal("replay returned a different order id")
		}
	}
}

func TestGetOrder(t *testing.T) {
	_, handler := newTestRouter(t)

	created := doJSON(handler, http.MethodPost, "/v1/orders", validOrder)
	order := decodeOrder(t, created)

	ok := doJSON(handler, http.MethodGet, "/v1/orders/"+order.ID+"?tenant_id=%20tenant-a%20", "")
	if ok.Code != http.StatusOK || ok.Body.String() != created.Body.String() {
		t.Fatalf("get status = %d body = %s, want identical stored order", ok.Code, ok.Body.String())
	}

	otherTenant := doJSON(handler, http.MethodGet, "/v1/orders/"+order.ID+"?tenant_id=someone-else", "")
	if otherTenant.Code != http.StatusNotFound {
		t.Fatalf("other tenant status = %d, want 404", otherTenant.Code)
	}
	assertErrorCode(t, otherTenant, "order_not_found")

	missing := doJSON(handler, http.MethodGet, "/v1/orders/does-not-exist?tenant_id=%20tenant-a%20", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want 404", missing.Code)
	}
	assertErrorCode(t, missing, "order_not_found")

	for _, path := range []string{
		"/v1/orders/" + order.ID,
		"/v1/orders/" + order.ID + "?tenant_id=",
		"/v1/orders/" + order.ID + "?tenant_id=%20%20",
	} {
		recorder := doJSON(handler, http.MethodGet, path, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status = %d, want 400", path, recorder.Code)
		}
		assertErrorCode(t, recorder, "invalid_request")
	}
}

func TestInvalidCreateRequests(t *testing.T) {
	_, handler := newTestRouter(t)

	cases := map[string]string{
		"empty body":              ``,
		"malformed json":          `{`,
		"json null":               `null`,
		"json array":              `[]`,
		"missing tenant_id":       `{"idempotency_key":"k","lines":[{"product_id":"p","quantity":1,"unit_price":1}]}`,
		"blank tenant_id":         `{"tenant_id":"   ","idempotency_key":"k","lines":[{"product_id":"p","quantity":1,"unit_price":1}]}`,
		"numeric tenant_id":       `{"tenant_id":1,"idempotency_key":"k","lines":[{"product_id":"p","quantity":1,"unit_price":1}]}`,
		"missing idempotency_key": `{"tenant_id":"t","lines":[{"product_id":"p","quantity":1,"unit_price":1}]}`,
		"blank idempotency_key":   `{"tenant_id":"t","idempotency_key":"  ","lines":[{"product_id":"p","quantity":1,"unit_price":1}]}`,
		"missing lines":           `{"tenant_id":"t","idempotency_key":"k"}`,
		"null lines":              `{"tenant_id":"t","idempotency_key":"k","lines":null}`,
		"empty lines":             `{"tenant_id":"t","idempotency_key":"k","lines":[]}`,
		"missing product_id":      `{"tenant_id":"t","idempotency_key":"k","lines":[{"quantity":1,"unit_price":1}]}`,
		"blank product_id":        `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"  ","quantity":1,"unit_price":1}]}`,
		"missing quantity":        `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","unit_price":1}]}`,
		"zero quantity":           `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":0,"unit_price":1}]}`,
		"negative quantity":       `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":-1,"unit_price":1}]}`,
		"fractional quantity":     `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":1.5,"unit_price":1}]}`,
		"string quantity":         `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":"1","unit_price":1}]}`,
		"missing unit_price":      `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":1}]}`,
		"negative unit_price":     `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":1,"unit_price":-1}]}`,
		"fractional unit_price":   `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":1,"unit_price":1.5}]}`,
		"quantity beyond int64":   `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":99999999999999999999999,"unit_price":1}]}`,
		"lines is an object":      `{"tenant_id":"t","idempotency_key":"k","lines":{"product_id":"p"}}`,
		"trailing json value":     `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":1,"unit_price":1}]}{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := doJSON(handler, http.MethodPost, "/v1/orders", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", recorder.Code, recorder.Body.String())
			}
			assertErrorCode(t, recorder, "invalid_request")
		})
	}
}

func TestTotalAmountOverflow(t *testing.T) {
	_, handler := newTestRouter(t)

	cases := map[string]string{
		"product overflows": `{"tenant_id":"t","idempotency_key":"k1","lines":[{"product_id":"p","quantity":9223372036854775807,"unit_price":2}]}`,
		"sum overflows":     `{"tenant_id":"t","idempotency_key":"k2","lines":[{"product_id":"p","quantity":9223372036854775807,"unit_price":1},{"product_id":"q","quantity":1,"unit_price":1}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := doJSON(handler, http.MethodPost, "/v1/orders", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", recorder.Code, recorder.Body.String())
			}
			assertErrorCode(t, recorder, "invalid_request")
		})
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "service.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first := doJSON(NewRouter(st), http.MethodPost, "/v1/orders", validOrder)
	if first.Code != http.StatusCreated {
		t.Fatalf("create status = %d", first.Code)
	}
	order := decodeOrder(t, first)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	handler2 := NewRouter(st2)

	fetched := doJSON(handler2, http.MethodGet, "/v1/orders/"+order.ID+"?tenant_id=%20tenant-a%20", "")
	if fetched.Code != http.StatusOK {
		t.Fatalf("after reopen get status = %d, want 200; body %s", fetched.Code, fetched.Body.String())
	}
	if fetched.Body.String() != first.Body.String() {
		t.Fatalf("after reopen body differs:\n%s\n%s", fetched.Body.String(), first.Body.String())
	}

	replay := doJSON(handler2, http.MethodPost, "/v1/orders", validOrder)
	if replay.Code != http.StatusOK || replay.Body.String() != first.Body.String() {
		t.Fatalf("after reopen replay status = %d, body %s", replay.Code, replay.Body.String())
	}
}

func TestStorageUnavailableOnClosedStore(t *testing.T) {
	st, handler := newTestRouter(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	recorder := doJSON(handler, http.MethodPost, "/v1/orders", validOrder)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body %s", recorder.Code, recorder.Body.String())
	}
	assertErrorCode(t, recorder, "storage_unavailable")
}

func TestErrorEnvelopeShape(t *testing.T) {
	_, handler := newTestRouter(t)

	recorder := doJSON(handler, http.MethodPost, "/v1/orders", `{`)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &top); err != nil {
		t.Fatalf("body is not a JSON object: %v", err)
	}
	if len(top) != 1 {
		t.Fatalf("top level has %d keys, want only error", len(top))
	}
	var envelope errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Error.Code == "" || envelope.Error.Message == "" {
		t.Fatalf("code and message must both be non-empty: %+v", envelope.Error)
	}
}

func assertErrorCode(t *testing.T, recorder *httptest.ResponseRecorder, code string) {
	t.Helper()
	var envelope errorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error body %q: %v", recorder.Body.String(), err)
	}
	if envelope.Error.Code != code {
		t.Fatalf("error code = %q, want %q; body %s", envelope.Error.Code, code, recorder.Body.String())
	}
	if envelope.Error.Message == "" {
		t.Fatal("error message is empty")
	}
}
