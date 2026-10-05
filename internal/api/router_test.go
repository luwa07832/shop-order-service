package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/shop-order-service/internal/store"
)

func TestHealthzReportsOK(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	NewRouter(st).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Body.String(); got != `{"database":"ok","status":"ok"}` {
		t.Fatalf("body = %s", got)
	}
}

func TestUnknownRouteUsesPublishedErrorShape(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/missing", nil)
	NewRouter(st).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func newTestRouter(t *testing.T) (*gin.Engine, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "service.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewRouter(st), st
}

func postOrder(t *testing.T, router *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeOrder(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var order map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &order); err != nil {
		t.Fatalf("decode order: %v", err)
	}
	return order
}

const validOrderBody = `{
	"tenant_id": "tenant-a",
	"idempotency_key": "key-1",
	"lines": [
		{"product_id": "p-1", "quantity": 2, "unit_price": 500},
		{"product_id": "p-2", "quantity": 1, "unit_price": 300}
	]
}`

func TestCreateOrderReturns201(t *testing.T) {
	router, _ := newTestRouter(t)

	recorder := postOrder(t, router, validOrderBody)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
	}
	order := decodeOrder(t, recorder)
	if order["id"] == "" || order["status"] != "created" || order["created_at"] == "" {
		t.Fatalf("order = %v", order)
	}
	if order["tenant_id"] != "tenant-a" || order["idempotency_key"] != "key-1" {
		t.Fatalf("order = %v", order)
	}
	if order["total_amount"] != float64(1300) {
		t.Fatalf("total_amount = %v", order["total_amount"])
	}
	lines, ok := order["lines"].([]any)
	if !ok || len(lines) != 2 {
		t.Fatalf("lines = %v", order["lines"])
	}
	first := lines[0].(map[string]any)
	if first["product_id"] != "p-1" || first["quantity"] != float64(2) || first["unit_price"] != float64(500) {
		t.Fatalf("first line = %v", first)
	}
}

func TestIdempotentReplayReturns200WithOriginalOrder(t *testing.T) {
	router, _ := newTestRouter(t)

	created := postOrder(t, router, validOrderBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d", created.Code)
	}
	original := decodeOrder(t, created)

	// Same content, different field order, extra unknown field, different whitespace.
	replayed := postOrder(t, router, `{"extra": true, "lines":[
		{"unit_price":500,"quantity":2,"product_id":"p-1"},
		{"product_id":"p-2","quantity":1,"unit_price":300,"ignored":"x"}
	],"idempotency_key":"key-1","tenant_id":"tenant-a"}`)
	if replayed.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body = %s", replayed.Code, replayed.Body)
	}
	again := decodeOrder(t, replayed)
	if again["id"] != original["id"] || again["created_at"] != original["created_at"] {
		t.Fatalf("replay = %v, original = %v", again, original)
	}
}

func TestIdempotencyConflictReturns409(t *testing.T) {
	router, _ := newTestRouter(t)

	if recorder := postOrder(t, router, validOrderBody); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d", recorder.Code)
	}
	conflict := postOrder(t, router, `{
		"tenant_id": "tenant-a",
		"idempotency_key": "key-1",
		"lines": [{"product_id": "p-1", "quantity": 3, "unit_price": 500}]
	}`)
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d, body = %s", conflict.Code, conflict.Body)
	}
	assertErrorBody(t, conflict, "idempotency_conflict")

	// Line order participates in the comparison.
	swapped := postOrder(t, router, `{
		"tenant_id": "tenant-a",
		"idempotency_key": "key-1",
		"lines": [
			{"product_id": "p-2", "quantity": 1, "unit_price": 300},
			{"product_id": "p-1", "quantity": 2, "unit_price": 500}
		]
	}`)
	if swapped.Code != http.StatusConflict {
		t.Fatalf("swapped status = %d", swapped.Code)
	}

	// The original order is unchanged.
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/orders/"+orderID(t, router)+"?tenant_id=tenant-a", nil)
	router.ServeHTTP(recorder, request)
	order := decodeOrder(t, recorder)
	if order["total_amount"] != float64(1300) {
		t.Fatalf("total_amount = %v", order["total_amount"])
	}
}

func TestSameKeyReusableAcrossTenants(t *testing.T) {
	router, _ := newTestRouter(t)

	if recorder := postOrder(t, router, validOrderBody); recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d", recorder.Code)
	}
	other := postOrder(t, router, `{
		"tenant_id": "tenant-b",
		"idempotency_key": "key-1",
		"lines": [{"product_id": "p-9", "quantity": 1, "unit_price": 100}]
	}`)
	if other.Code != http.StatusCreated {
		t.Fatalf("other tenant status = %d, body = %s", other.Code, other.Body)
	}
}

func TestGetOrder(t *testing.T) {
	router, _ := newTestRouter(t)

	created := postOrder(t, router, validOrderBody)
	id := decodeOrder(t, created)["id"].(string)

	get := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		router.ServeHTTP(recorder, request)
		return recorder
	}

	ok := get("/v1/orders/" + id + "?tenant_id=tenant-a")
	if ok.Code != http.StatusOK {
		t.Fatalf("get status = %d, body = %s", ok.Code, ok.Body)
	}
	if decodeOrder(t, ok)["id"] != id {
		t.Fatalf("get body = %s", ok.Body)
	}

	if recorder := get("/v1/orders/" + id + "?tenant_id=tenant-b"); recorder.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status = %d", recorder.Code)
	} else {
		assertErrorBody(t, recorder, "order_not_found")
	}
	if recorder := get("/v1/orders/nope?tenant_id=tenant-a"); recorder.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d", recorder.Code)
	}
	if recorder := get("/v1/orders/" + id); recorder.Code != http.StatusBadRequest {
		t.Fatalf("no tenant status = %d", recorder.Code)
	} else {
		assertErrorBody(t, recorder, "invalid_request")
	}
	if recorder := get("/v1/orders/" + id + "?tenant_id=%20%20"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("blank tenant status = %d", recorder.Code)
	}
}

func TestInvalidCreateRequests(t *testing.T) {
	router, _ := newTestRouter(t)

	cases := map[string]string{
		"not json":            `{`,
		"trailing data":       validOrderBody + ` {}`,
		"array body":          `[]`,
		"missing tenant":      `{"idempotency_key":"k","lines":[{"product_id":"p","quantity":1,"unit_price":1}]}`,
		"blank tenant":        `{"tenant_id":"  ","idempotency_key":"k","lines":[{"product_id":"p","quantity":1,"unit_price":1}]}`,
		"wrong tenant type":   `{"tenant_id":7,"idempotency_key":"k","lines":[{"product_id":"p","quantity":1,"unit_price":1}]}`,
		"missing key":         `{"tenant_id":"t","lines":[{"product_id":"p","quantity":1,"unit_price":1}]}`,
		"empty lines":         `{"tenant_id":"t","idempotency_key":"k","lines":[]}`,
		"missing lines":       `{"tenant_id":"t","idempotency_key":"k"}`,
		"blank product":       `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":" ","quantity":1,"unit_price":1}]}`,
		"zero quantity":       `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":0,"unit_price":1}]}`,
		"negative quantity":   `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":-1,"unit_price":1}]}`,
		"fractional quantity": `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":1.5,"unit_price":1}]}`,
		"string quantity":     `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":"1","unit_price":1}]}`,
		"negative price":      `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":1,"unit_price":-1}]}`,
		"missing price":       `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":1}]}`,
		"multiply overflow":   `{"tenant_id":"t","idempotency_key":"k","lines":[{"product_id":"p","quantity":9223372036854775807,"unit_price":2}]}`,
		"sum overflow": `{"tenant_id":"t","idempotency_key":"k","lines":[
			{"product_id":"p","quantity":9223372036854775807,"unit_price":1},
			{"product_id":"q","quantity":1,"unit_price":1}
		]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			recorder := postOrder(t, router, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
			}
			assertErrorBody(t, recorder, "invalid_request")
		})
	}
}

func TestConcurrentSameContentCreatesOneOrder(t *testing.T) {
	router, _ := newTestRouter(t)

	const workers = 8
	recorders := make([]*httptest.ResponseRecorder, workers)
	var wg sync.WaitGroup
	for i := range recorders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorders[i] = httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/orders", strings.NewReader(validOrderBody))
			router.ServeHTTP(recorders[i], request)
		}()
	}
	wg.Wait()

	ids := map[string]int{}
	creates := 0
	for _, recorder := range recorders {
		if recorder.Code != http.StatusCreated && recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body)
		}
		if recorder.Code == http.StatusCreated {
			creates++
		}
		ids[decodeOrder(t, recorder)["id"].(string)]++
	}
	if creates != 1 || len(ids) != 1 {
		t.Fatalf("creates = %d, distinct ids = %d", creates, len(ids))
	}
}

func TestOrdersPersistAcrossRouterRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	created := postOrder(t, NewRouter(st), validOrderBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d", created.Code)
	}
	original := decodeOrder(t, created)
	st.Close()

	reopened, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	router := NewRouter(reopened)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/orders/"+original["id"].(string)+"?tenant_id=tenant-a", nil)
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || decodeOrder(t, recorder)["created_at"] != original["created_at"] {
		t.Fatalf("get after reopen: %d %s", recorder.Code, recorder.Body)
	}

	replayed := postOrder(t, router, validOrderBody)
	if replayed.Code != http.StatusOK || decodeOrder(t, replayed)["id"] != original["id"] {
		t.Fatalf("replay after reopen: %d %s", replayed.Code, replayed.Body)
	}
}

func assertErrorBody(t *testing.T, recorder *httptest.ResponseRecorder, code string) {
	t.Helper()
	var body map[string]map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	errObj, ok := body["error"]
	if !ok || len(body) != 1 {
		t.Fatalf("body = %s", recorder.Body)
	}
	if errObj["code"] != code || errObj["message"] == "" {
		t.Fatalf("error = %v, want code %s", errObj, code)
	}
}

// orderID creates the sample order and returns its id.
func orderID(t *testing.T, router *gin.Engine) string {
	t.Helper()
	recorder := postOrder(t, router, validOrderBody)
	if recorder.Code != http.StatusCreated && recorder.Code != http.StatusOK {
		t.Fatalf("create status = %d", recorder.Code)
	}
	return decodeOrder(t, recorder)["id"].(string)
}
