package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func testApp(t *testing.T) *App {
	t.Helper()
	a, err := NewApp(filepath.Join(t.TempDir(), "catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func one(sku, name string, price float64, version int64, withdrawn bool) []ProductUpdate {
	return []ProductUpdate{{SKU: sku, Name: name, Price: price, Version: version, Withdrawn: withdrawn}}
}
func productBySKU(t *testing.T, a *App, sku string) Product {
	t.Helper()
	for _, p := range a.Products() {
		if p.SKU == sku {
			return p
		}
	}
	t.Fatalf("product %s not found", sku)
	return Product{}
}
func TestImportIsIdempotent(t *testing.T) {
	a := testApp(t)
	records := one("SKU-1", "Lamp", 10, 1, false)
	b1, err := a.Import("batch-1", records)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := a.Import("batch-1", records)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Products()) != 1 {
		t.Fatalf("want 1 product, got %d", len(a.Products()))
	}
	if b1.Processed != 1 || b2.Processed != 1 || b2.Status != "Completed" {
		t.Fatalf("unexpected batches: %+v %+v", b1, b2)
	}
}
func TestOldVersionCannotOverwriteNewer(t *testing.T) {
	a := testApp(t)
	if _, err := a.Import("new", one("SKU-1", "Lamp", 20, 2, false)); err != nil {
		t.Fatal(err)
	}
	b, err := a.Import("old", one("SKU-1", "Old Lamp", 10, 1, false))
	if err != nil {
		t.Fatal(err)
	}
	p := productBySKU(t, a, "SKU-1")
	if p.Version != 2 || p.Price != 20 || p.Name != "Lamp" {
		t.Fatalf("old update overwrote product: %+v", p)
	}
	if b.Skipped != 1 {
		t.Fatalf("expected old record skipped, got %+v", b)
	}
}
func TestWithdrawnProductIsNotRestoredByOldUpdate(t *testing.T) {
	a := testApp(t)
	if _, err := a.Import("create", one("SKU-1", "Lamp", 10, 1, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Import("withdraw", one("SKU-1", "Lamp", 10, 2, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Import("late-old", one("SKU-1", "Lamp", 10, 1, false)); err != nil {
		t.Fatal(err)
	}
	p := productBySKU(t, a, "SKU-1")
	if !p.Withdrawn || p.Version != 2 {
		t.Fatalf("withdrawn product incorrectly restored: %+v", p)
	}
}
func TestRecoveryAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.json")
	a, err := NewApp(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an interrupted batch persisted after its first record.
	records := []ProductUpdate{{SKU: "SKU-1", Name: "Lamp", Price: 10, Version: 1}, {SKU: "SKU-2", Name: "Chair", Price: 20, Version: 1}}
	b := Batch{ID: "interrupted", Records: records, Status: "In Progress", NextIndex: 1, Processed: 1, PayloadHash: hashPayload(records)}
	a.db.Products["SKU-1"] = Product{SKU: "SKU-1", Name: "Lamp", Price: 10, Version: 1}
	a.db.Batches[b.ID] = b
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewApp(path)
	if err != nil {
		t.Fatal(err)
	}
	got := restarted.db.Batches["interrupted"]
	if got.Status != "Pending" || got.NextIndex != 1 {
		t.Fatalf("not recovered to pending at index 1: %+v", got)
	}
	got, err = restarted.processBatch(got)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "Completed" || len(restarted.Products()) != 2 {
		t.Fatalf("batch did not resume: %+v products=%d", got, len(restarted.Products()))
	}
}
func TestBatchIDCannotBeReusedWithDifferentPayload(t *testing.T) {
	a := testApp(t)
	if _, err := a.Import("same-id", one("SKU-1", "Lamp", 10, 1, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Import("same-id", one("SKU-1", "Lamp", 12, 2, false)); err == nil {
		t.Fatal("expected different payload to be rejected")
	}
}
func TestDuplicateSKUInOneBatchRejected(t *testing.T) {
	a := testApp(t)
	records := []ProductUpdate{{SKU: "SKU-1", Name: "A", Price: 1, Version: 1}, {SKU: "SKU-1", Name: "B", Price: 2, Version: 2}}
	if _, err := a.Import("dupes", records); err == nil {
		t.Fatal("expected duplicate SKU to be rejected")
	}
}

func TestEmptyBatchRejected(t *testing.T) {
	a := testApp(t)

	if _, err := a.Import("empty", []ProductUpdate{}); err == nil {
		t.Fatal("expected empty batch to be rejected")
	}
}

func TestBlankSKURejected(t *testing.T) {
	a := testApp(t)

	if _, err := a.Import("blank-sku", one("   ", "Lamp", 10, 1, false)); err == nil {
		t.Fatal("expected blank SKU to be rejected")
	}
}

func TestInvalidVersionRejected(t *testing.T) {
	a := testApp(t)

	if _, err := a.Import("invalid-version", one("SKU-1", "Lamp", 10, 0, false)); err == nil {
		t.Fatal("expected version 0 to be rejected")
	}
}

func TestNegativePriceRejected(t *testing.T) {
	a := testApp(t)

	if _, err := a.Import("negative-price", one("SKU-1", "Lamp", -1, 1, false)); err == nil {
		t.Fatal("expected negative price to be rejected")
	}
}

func TestBlankNameRejectedForActiveProduct(t *testing.T) {
	a := testApp(t)

	if _, err := a.Import("blank-name", one("SKU-1", "   ", 10, 1, false)); err == nil {
		t.Fatal("expected blank name to be rejected for active product")
	}
}

func TestSKUIsTrimmedBeforeImport(t *testing.T) {
	a := testApp(t)

	_, err := a.Import("trim-sku", one(" SKU-1 ", "Lamp", 10, 1, false))
	if err != nil {
		t.Fatal(err)
	}

	if len(a.Products()) != 1 {
		t.Fatalf("expected 1 product, got %d", len(a.Products()))
	}

	if got := a.Products()[0].SKU; got != "SKU-1" {
		t.Fatalf("expected trimmed SKU %q, got %q", "SKU-1", got)
	}
}

func TestImportEndpointRejectsTrailingJSON(t *testing.T) {
	a := testApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/import",
		strings.NewReader(`{"batchId":"batch-1","records":[{"sku":"SKU-1","name":"Lamp","price":10,"version":1}]} {}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	a.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if len(a.Products()) != 0 {
		t.Fatalf("invalid request must not import products, got %d", len(a.Products()))
	}
}

func TestImportEndpointRejectsUnknownFields(t *testing.T) {
	a := testApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/import",
		strings.NewReader(`{"batchId":"batch-1","records":[{"sku":"SKU-1","name":"Lamp","price":10,"version":1}],"unexpected":true}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	a.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestHealthEndpointRejectsUnsupportedMethod(t *testing.T) {
	a := testApp(t)
	req := httptest.NewRequest(http.MethodPost, "/api/health", nil)
	rec := httptest.NewRecorder()

	a.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
}

func TestImportEndpointRejectsOversizedBody(t *testing.T) {
	a := testApp(t)
	body := `{"batchId":"batch-1","records":[],"padding":"` + strings.Repeat("x", (1<<20)+1) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	a.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d", http.StatusRequestEntityTooLarge, rec.Code)
	}
}