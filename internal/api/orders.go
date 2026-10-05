package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/luwa07832/shop-order-service/internal/store"
)

// createOrderRequest is the wire shape of POST /v1/orders. Pointers let the
// handler distinguish an omitted field (invalid) from an explicit zero value
// (unit_price of 0 is valid). Unknown fields are ignored by encoding/json.
type createOrderRequest struct {
	TenantID       *string           `json:"tenant_id"`
	IdempotencyKey *string           `json:"idempotency_key"`
	Lines          *[]orderLineInput `json:"lines"`
}

type orderLineInput struct {
	ProductID *string `json:"product_id"`
	Quantity  *int64  `json:"quantity"`
	UnitPrice *int64  `json:"unit_price"`
}

// canonicalLine is marshaled with a fixed field order to fingerprint content;
// line order is preserved by marshaling the slice in submission sequence.
type canonicalLine struct {
	ProductID string `json:"product_id"`
	Quantity  int64  `json:"quantity"`
	UnitPrice int64  `json:"unit_price"`
}

func createOrder(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		req, ok := decodeCreateRequest(c)
		if !ok {
			return
		}

		params, ok := validateCreateRequest(c, req)
		if !ok {
			return
		}

		order, replay, err := st.CreateOrder(c.Request.Context(), params)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrConflict):
				writeError(c, http.StatusConflict, "idempotency_conflict",
					"the idempotency key was already used with different order content")
			case errors.Is(err, store.ErrUnavailable):
				writeError(c, http.StatusServiceUnavailable, "storage_unavailable",
					"storage is not available")
			default:
				writeError(c, http.StatusServiceUnavailable, "storage_unavailable",
					"storage is not available")
			}
			return
		}

		status := http.StatusCreated
		if replay {
			status = http.StatusOK
		}
		c.JSON(status, order)
	}
}

func getOrder(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.Query("tenant_id")
		if strings.TrimSpace(tenantID) == "" {
			writeError(c, http.StatusBadRequest, "invalid_request",
				"tenant_id must be a non-blank string")
			return
		}
		order, err := st.OrderByID(c.Request.Context(), tenantID, c.Param("id"))
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				writeError(c, http.StatusNotFound, "order_not_found", "the order does not exist")
			default:
				writeError(c, http.StatusServiceUnavailable, "storage_unavailable",
					"storage is not available")
			}
			return
		}
		c.JSON(http.StatusOK, order)
	}
}

// decodeCreateRequest parses exactly one JSON value; trailing data and any
// type mismatch are invalid requests.
func decodeCreateRequest(c *gin.Context) (createOrderRequest, bool) {
	var req createOrderRequest
	decoder := json.NewDecoder(c.Request.Body)
	if err := decoder.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request",
			"request body must be a valid JSON order object")
		return req, false
	}
	// Reject trailing JSON values or garbage after the first object.
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		writeError(c, http.StatusBadRequest, "invalid_request",
			"request body must contain exactly one JSON object")
		return req, false
	}
	return req, true
}

func validateCreateRequest(c *gin.Context, req createOrderRequest) (store.CreateOrderParams, bool) {
	params := store.CreateOrderParams{}
	if req.TenantID == nil || strings.TrimSpace(*req.TenantID) == "" {
		invalidRequest(c, "tenant_id must be a non-blank string")
		return params, false
	}
	if req.IdempotencyKey == nil || strings.TrimSpace(*req.IdempotencyKey) == "" {
		invalidRequest(c, "idempotency_key must be a non-blank string")
		return params, false
	}
	params.TenantID = *req.TenantID
	params.IdempotencyKey = *req.IdempotencyKey

	if req.Lines == nil || len(*req.Lines) == 0 {
		invalidRequest(c, "lines must be a non-empty array")
		return params, false
	}

	canonical := make([]canonicalLine, 0, len(*req.Lines))
	lines := make([]store.Line, 0, len(*req.Lines))
	var total int64
	for _, line := range *req.Lines {
		if line.ProductID == nil || strings.TrimSpace(*line.ProductID) == "" {
			invalidRequest(c, "each line needs a non-blank product_id")
			return params, false
		}
		if line.Quantity == nil || *line.Quantity <= 0 {
			invalidRequest(c, "quantity must be a positive integer")
			return params, false
		}
		if line.UnitPrice == nil || *line.UnitPrice < 0 {
			invalidRequest(c, "unit_price must be a non-negative integer")
			return params, false
		}
		quantity, unitPrice := *line.Quantity, *line.UnitPrice
		product, overflow := multiplyChecked(quantity, unitPrice)
		if overflow {
			invalidRequest(c, "total_amount exceeds the signed 64-bit integer range")
			return params, false
		}
		if product > math.MaxInt64-total {
			invalidRequest(c, "total_amount exceeds the signed 64-bit integer range")
			return params, false
		}
		total += product
		lines = append(lines, store.Line{
			ProductID: *line.ProductID,
			Quantity:  quantity,
			UnitPrice: unitPrice,
		})
		canonical = append(canonical, canonicalLine{
			ProductID: *line.ProductID,
			Quantity:  quantity,
			UnitPrice: unitPrice,
		})
	}
	params.Lines = lines
	params.TotalAmount = total
	params.Fingerprint = fingerprint(canonical)
	return params, true
}

// multiplyChecked returns q*u and whether the product overflows int64. q > 0
// and u >= 0 are guaranteed by validation.
func multiplyChecked(q, u int64) (int64, bool) {
	if u == 0 {
		return 0, false
	}
	if q > math.MaxInt64/u {
		return 0, true
	}
	return q * u, false
}

func fingerprint(lines []canonicalLine) string {
	encoded, err := json.Marshal(lines)
	if err != nil {
		// All fields are plain strings and integers; this cannot fail.
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func invalidRequest(c *gin.Context, message string) {
	writeError(c, http.StatusBadRequest, "invalid_request", message)
}

func writeError(c *gin.Context, status int, code, message string) {
	// Fixed, storage-agnostic wording: no SQL, stack traces, or paths.
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
