package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/luwa07832/shop-order-service/internal/store"
)

// createOrderLine is the submitted shape of one order line. Numbers stay raw
// so non-integer or wrongly typed values fail validation.
type createOrderLine struct {
	ProductID string          `json:"product_id"`
	Quantity  json.RawMessage `json:"quantity"`
	UnitPrice json.RawMessage `json:"unit_price"`
}

// createOrderRequest is the submitted shape of POST /v1/orders. Unknown fields
// are ignored by the decoder, per the service contract.
type createOrderRequest struct {
	TenantID       string            `json:"tenant_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	Lines          []createOrderLine `json:"lines"`
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}

func invalidRequest(c *gin.Context) {
	writeError(c, http.StatusBadRequest, "invalid_request", "request body or parameters are invalid")
}

// validIdentifier reports whether s is non-empty and not all whitespace. The
// original string, whitespace included, is what gets stored.
func validIdentifier(s string) bool { return strings.TrimSpace(s) != "" }

func createOrderHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		var payload createOrderRequest
		decoder := json.NewDecoder(c.Request.Body)
		if err := decoder.Decode(&payload); err != nil {
			invalidRequest(c)
			return
		}
		// The body must be a single JSON value; trailing data is invalid.
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			invalidRequest(c)
			return
		}

		if !validIdentifier(payload.TenantID) || !validIdentifier(payload.IdempotencyKey) || len(payload.Lines) == 0 {
			invalidRequest(c)
			return
		}

		lines := make([]store.Line, 0, len(payload.Lines))
		var total int64
		for _, raw := range payload.Lines {
			if !validIdentifier(raw.ProductID) {
				invalidRequest(c)
				return
			}
			quantity, err := strconv.ParseInt(string(raw.Quantity), 10, 64)
			if err != nil || quantity <= 0 {
				invalidRequest(c)
				return
			}
			unitPrice, err := strconv.ParseInt(string(raw.UnitPrice), 10, 64)
			if err != nil || unitPrice < 0 {
				invalidRequest(c)
				return
			}
			if unitPrice > 0 && quantity > math.MaxInt64/unitPrice {
				invalidRequest(c)
				return
			}
			lineTotal := quantity * unitPrice
			if total > math.MaxInt64-lineTotal {
				invalidRequest(c)
				return
			}
			total += lineTotal
			lines = append(lines, store.Line{ProductID: raw.ProductID, Quantity: quantity, UnitPrice: unitPrice})
		}

		order := store.Order{
			ID:             uuid.NewString(),
			TenantID:       payload.TenantID,
			IdempotencyKey: payload.IdempotencyKey,
			Lines:          lines,
			TotalAmount:    total,
			Status:         "created",
			CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		}

		stored, created, err := st.CreateOrder(order, hashContent(lines))
		switch {
		case errors.Is(err, store.ErrContentConflict):
			writeError(c, http.StatusConflict, "idempotency_conflict", "idempotency key was already used with different order content")
		case err != nil:
			writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "database is not available")
		case created:
			c.JSON(http.StatusCreated, stored)
		default:
			c.JSON(http.StatusOK, stored)
		}
	}
}

func getOrderHandler(st *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		tenantID := c.Query("tenant_id")
		if !validIdentifier(tenantID) {
			invalidRequest(c)
			return
		}
		order, found, err := st.GetOrder(tenantID, c.Param("id"))
		if err != nil {
			writeError(c, http.StatusServiceUnavailable, "storage_unavailable", "database is not available")
			return
		}
		if !found {
			writeError(c, http.StatusNotFound, "order_not_found", "no order matches this id for the tenant")
			return
		}
		c.JSON(http.StatusOK, order)
	}
}

// hashContent fingerprints the order content that participates in idempotency
// comparison: the ordered lines. Field order and whitespace of the original
// JSON never reach this point, and unknown fields were already dropped.
func hashContent(lines []store.Line) string {
	digest := sha256.New()
	for _, line := range lines {
		fmt.Fprintf(digest, "%s\x00%d\x00%d\x00", line.ProductID, line.Quantity, line.UnitPrice)
	}
	return hex.EncodeToString(digest.Sum(nil))
}
