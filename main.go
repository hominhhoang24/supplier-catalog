package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ProductUpdate struct {
	SKU       string  `json:"sku"`
	Name      string  `json:"name"`
	Price     float64 `json:"price"`
	Version   int64   `json:"version"`
	Withdrawn bool    `json:"withdrawn"`
}

type Product struct {
	SKU       string    `json:"sku"`
	Name      string    `json:"name"`
	Price     float64   `json:"price"`
	Version   int64     `json:"version"`
	Withdrawn bool      `json:"withdrawn"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type Batch struct {
	ID          string          `json:"id"`
	Records     []ProductUpdate `json:"records"`
	Status      string          `json:"status"`
	NextIndex   int             `json:"nextIndex"`
	Processed   int             `json:"processed"`
	Skipped     int             `json:"skipped"`
	Error       string          `json:"error,omitempty"`
	PayloadHash string          `json:"payloadHash"`
	CreatedAt   time.Time       `json:"createdAt"`
	UpdatedAt   time.Time       `json:"updatedAt"`
}

type database struct {
	Products map[string]Product `json:"products"`
	Batches  map[string]Batch   `json:"batches"`
}

type App struct {
	mu   sync.Mutex
	path string
	db   database
}

func NewApp(path string) (*App, error) {
	a := &App{path: path, db: database{Products: map[string]Product{}, Batches: map[string]Batch{}}}
	if err := a.load(); err != nil {
		return nil, err
	}
	// Any batch left in progress by a previous process is resumable.
	for id, b := range a.db.Batches {
		if b.Status == "In Progress" {
			b.Status = "Pending"
			b.UpdatedAt = time.Now().UTC()
			a.db.Batches[id] = b
		}
	}
	if err := a.save(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) load() error {
	raw, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var d database
	if err := json.Unmarshal(raw, &d); err != nil {
		return fmt.Errorf("đọc JSON cơ sở dữ liệu: %w", err)
	}
	if d.Products == nil {
		d.Products = map[string]Product{}
	}
	if d.Batches == nil {
		d.Batches = map[string]Batch{}
	}
	a.db = d
	return nil
}

func (a *App) save() error {
	if err := os.MkdirAll(filepath.Dir(a.path), 0755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(a.db, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

func hashPayload(records []ProductUpdate) string {
	raw, _ := json.Marshal(records)
	// A stable simple hash is sufficient for this local demonstration.
	var h uint64 = 14695981039346656037
	for _, b := range raw {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return fmt.Sprintf("%x", h)
}

func validate(records []ProductUpdate) error {
	if len(records) == 0 {
		return errors.New("lô dữ liệu phải có ít nhất một bản ghi")
	}

	seen := map[string]bool{}

	for i := range records {
		records[i].SKU = strings.TrimSpace(records[i].SKU)
		r := records[i]

		if r.SKU == "" {
			return fmt.Errorf("bản ghi %d: bắt buộc phải có SKU", i+1)
		}

		if seen[r.SKU] {
			return fmt.Errorf("SKU %q bị trùng trong cùng một lô", r.SKU)
		}
		seen[r.SKU] = true

		if r.Version < 1 {
			return fmt.Errorf("bản ghi %d (%s): phiên bản phải lớn hơn hoặc bằng 1", i+1, r.SKU)
		}

		if !r.Withdrawn && strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("bản ghi %d (%s): bắt buộc phải có tên sản phẩm", i+1, r.SKU)
		}

		if r.Price < 0 {
			return fmt.Errorf("bản ghi %d (%s): giá không được âm", i+1, r.SKU)
		}
	}

	return nil
}

func (a *App) Import(id string, records []ProductUpdate) (Batch, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return Batch{}, errors.New("bắt buộc phải có mã lô")
	}
	if err := validate(records); err != nil {
		return Batch{}, err
	}
	hash := hashPayload(records)
	if existing, ok := a.db.Batches[id]; ok {
		if existing.PayloadHash != hash {
			return Batch{}, fmt.Errorf("mã lô %q đã tồn tại với dữ liệu khác", id)
		}
		if existing.Status == "Completed" {
			return existing, nil
		}
		return a.processBatch(existing)
	}
	now := time.Now().UTC()
	b := Batch{ID: id, Records: records, Status: "Pending", PayloadHash: hash, CreatedAt: now, UpdatedAt: now}
	a.db.Batches[id] = b
	if err := a.save(); err != nil {
		return Batch{}, err
	}
	return a.processBatch(b)
}

func (a *App) processBatch(b Batch) (Batch, error) {
	b.Status = "In Progress"
	b.Error = ""
	a.db.Batches[b.ID] = b
	if err := a.save(); err != nil {
		return Batch{}, err
	}

	for b.NextIndex < len(b.Records) {
		r := b.Records[b.NextIndex]
		current, exists := a.db.Products[r.SKU]
		switch {
		case !exists:
			a.db.Products[r.SKU] = Product{SKU: r.SKU, Name: r.Name, Price: r.Price, Version: r.Version, Withdrawn: r.Withdrawn, UpdatedAt: time.Now().UTC()}
			b.Processed++
		case r.Version > current.Version:
			// Newer versions may intentionally restore a withdrawn product.
			a.db.Products[r.SKU] = Product{SKU: r.SKU, Name: r.Name, Price: r.Price, Version: r.Version, Withdrawn: r.Withdrawn, UpdatedAt: time.Now().UTC()}
			b.Processed++
		default:
			// Same or older versions are ignored; tombstone remains.
			b.Skipped++
		}
		b.NextIndex++
		b.UpdatedAt = time.Now().UTC()
		a.db.Batches[b.ID] = b
		if err := a.save(); err != nil {
			b.Status = "In Progress"
			b.Error = err.Error()
			a.db.Batches[b.ID] = b
			return b, err
		}
	}
	b.Status = "Completed"
	b.UpdatedAt = time.Now().UTC()
	a.db.Batches[b.ID] = b
	if err := a.save(); err != nil {
		return b, err
	}
	return b, nil
}

func (a *App) Products() []Product {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]Product, 0, len(a.db.Products))
	for _, p := range a.db.Products {
		out = append(out, p)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].SKU < out[j].SKU
	})

	return out
}

// Batches returns batches in a stable order so the UI does not jump between refreshes.
func (a *App) Batches() []Batch {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make([]Batch, 0, len(a.db.Batches))
	for _, b := range a.db.Batches {
		out = append(out, b)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})

	return out
}

const page = `<!doctype html>
<html lang="vi"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="theme-color" content="#f5f7fb"><title>Quản lý danh mục nhà cung cấp</title>
<style>
:root{color-scheme:light;--ink:#172033;--muted:#758096;--line:#e6eaf1;--blue:#3867e8;--blue-dark:#244cc2;--canvas:#f5f7fb;--green:#13865b;--amber:#a9680b;--red:#c33d4b;--nav:#17243b;--shadow:0 8px 28px rgba(25,42,76,.045)}
*{box-sizing:border-box}html{scroll-behavior:smooth}body{margin:0;background:var(--canvas);color:var(--ink);font:14px/1.5 Inter,ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}button,input,textarea{font:inherit}button{cursor:pointer}.app{min-height:100vh}.sidebar{position:fixed;inset:0 auto 0 0;width:238px;background:var(--nav);color:#eaf0ff;padding:25px 16px;display:flex;flex-direction:column;z-index:5}.brand{display:flex;align-items:center;gap:11px;padding:0 10px 29px;font-size:16px;font-weight:750;letter-spacing:-.3px}.brand-mark{width:36px;height:36px;border-radius:11px;background:linear-gradient(145deg,#7095ff,#3867e8);display:grid;place-items:center;color:#fff;font-size:19px;box-shadow:0 6px 15px #0d173a}.brand small{display:block;color:#8fa0bf;font-size:10px;font-weight:600;letter-spacing:1.5px;text-transform:uppercase;margin-top:1px}.nav-label{padding:10px 12px;color:#8090ad;font-size:10px;font-weight:750;letter-spacing:1.5px;text-transform:uppercase}.nav-link{display:flex;align-items:center;gap:12px;padding:11px 13px;margin:3px 0;border-radius:9px;color:#b9c5dc;text-decoration:none;font-weight:600;transition:.15s}.nav-link:hover,.nav-link.active{background:#293a59;color:white}.nav-link.active{box-shadow:inset 3px 0 #7395ff}.nav-icon{width:19px;text-align:center;font-size:16px}.side-bottom{margin-top:auto;padding:16px 12px 0;border-top:1px solid #30405a;color:#8fa0bf;font-size:12px}.online-dot{display:inline-block;width:7px;height:7px;border-radius:50%;background:#3bd49c;margin-right:7px}.main{margin-left:238px;min-height:100vh}.topbar{height:72px;background:#fff;border-bottom:1px solid var(--line);display:flex;align-items:center;justify-content:space-between;padding:0 36px;gap:18px}.crumb{color:var(--muted);font-size:12px}.crumb strong{color:var(--ink);font-weight:650}.top-right{display:flex;align-items:center;gap:14px}.system-status{display:flex;align-items:center;gap:8px;padding:7px 11px;border:1px solid var(--line);border-radius:8px;color:#536078;font-size:12px;font-weight:600}.avatar{width:34px;height:34px;border-radius:50%;background:#e8edff;color:#3556b8;display:grid;place-items:center;font-weight:750}.content{max-width:1500px;margin:0 auto;padding:31px 36px 44px}.page-head{display:flex;align-items:flex-start;justify-content:space-between;gap:18px;margin-bottom:25px}.eyebrow{font-size:11px;font-weight:750;color:var(--blue);letter-spacing:1.5px;text-transform:uppercase;margin:0 0 6px}.page-head h1{font-size:27px;letter-spacing:-.8px;line-height:1.2;margin:0 0 8px}.subtitle{margin:0;color:var(--muted);font-size:13px}.btn{border:1px solid var(--line);background:#fff;color:#34415a;padding:10px 14px;border-radius:9px;font-weight:650;display:inline-flex;align-items:center;justify-content:center;gap:8px;transition:.15s;white-space:nowrap}.btn:hover{border-color:#b7c5e8;background:#f9faff}.btn-primary{background:var(--blue);border-color:var(--blue);color:#fff;box-shadow:0 4px 10px #3867e826}.btn-primary:hover{background:var(--blue-dark);border-color:var(--blue-dark);color:#fff}.btn:disabled{opacity:.65;cursor:wait}.metrics{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:16px;margin-bottom:24px}.metric{background:#fff;border:1px solid var(--line);border-radius:13px;padding:19px 20px;box-shadow:var(--shadow);min-width:0}.metric-top{display:flex;justify-content:space-between;align-items:center;gap:10px;color:var(--muted);font-size:12px;font-weight:650}.metric-icon{width:35px;height:35px;border-radius:10px;display:grid;place-items:center;font-size:16px;background:#edf2ff;color:var(--blue)}.metric:nth-child(2) .metric-icon{background:#e9f8f1;color:var(--green)}.metric:nth-child(3) .metric-icon{background:#fff4df;color:var(--amber)}.metric:nth-child(4) .metric-icon{background:#fcecef;color:var(--red)}.metric-value{font-size:29px;font-weight:760;letter-spacing:-1px;margin-top:13px;line-height:1.2}.metric-note{font-size:11px;color:var(--muted);margin-top:5px}.section-grid{display:grid;grid-template-columns:minmax(0,1.3fr) minmax(320px,.9fr);gap:18px;align-items:start}.panel{background:#fff;border:1px solid var(--line);border-radius:13px;box-shadow:var(--shadow);margin-bottom:19px;min-width:0;overflow:hidden}.panel-head{display:flex;justify-content:space-between;align-items:center;gap:14px;padding:19px 21px;border-bottom:1px solid var(--line)}.panel-title{font-size:15px;font-weight:750;margin:0;letter-spacing:-.2px}.panel-subtitle{font-size:12px;color:var(--muted);margin:4px 0 0}.panel-body{padding:20px 21px}.field-label{display:block;color:#47536a;font-size:12px;font-weight:700;margin-bottom:7px}.text-input,.search-input,.json-input{width:100%;border:1px solid #dce2ec;background:#fff;color:var(--ink);border-radius:8px;outline:none;transition:border .15s,box-shadow .15s}.text-input{padding:11px 12px}.text-input:focus,.search-input:focus,.json-input:focus{border-color:#7394f6;box-shadow:0 0 0 3px #3867e818}.field-help{font-size:11px;color:var(--muted);margin:6px 0 0}.form-row{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:12px;align-items:end;margin-bottom:17px}.json-label-row{display:flex;justify-content:space-between;align-items:center;gap:10px;margin-bottom:7px}.json-label-row .field-label{margin:0}.text-button{border:0;background:transparent;color:var(--blue);font-weight:700;font-size:11px;padding:3px 0}.json-input{display:block;min-height:214px;resize:vertical;padding:13px 14px;background:#111a2c;color:#dbe6ff;border-color:#111a2c;font:12px/1.7 ui-monospace,SFMono-Regular,Consolas,monospace;tab-size:2}.import-footer{display:flex;justify-content:space-between;align-items:center;gap:12px;margin-top:14px}.hint{font-size:11px;color:var(--muted);max-width:250px}.message{display:none;margin-top:14px;padding:11px 12px;border-radius:8px;font-size:12px;overflow-wrap:anywhere}.message.show{display:block}.message.success{background:#eaf8f1;color:#126c4a;border:1px solid #c8eedc}.message.error{background:#fff0f1;color:#a52e3a;border:1px solid #f5d1d5}.message.info{background:#eef3ff;color:#2f53b5;border:1px solid #d7e1ff}.table-tools{display:flex;gap:9px;align-items:center;padding:14px 20px;border-bottom:1px solid var(--line)}.search-wrap{position:relative;flex:1;min-width:120px}.search-input{padding:9px 11px 9px 34px;font-size:12px}.search-symbol{position:absolute;left:12px;top:8px;color:#8b96a9;font-size:14px}.select-input{border:1px solid #dce2ec;background:#fff;color:#536078;border-radius:8px;padding:9px 10px;font-size:12px;max-width:145px}.table-wrap{overflow:auto;width:100%}table{width:100%;border-collapse:collapse;white-space:nowrap}th{font-size:10px;text-transform:uppercase;letter-spacing:.8px;color:#8791a3;background:#fafbfe;font-weight:750;text-align:left;padding:12px 17px;border-bottom:1px solid var(--line)}td{padding:13px 17px;border-bottom:1px solid #eff1f6;font-size:12px;color:#47536a}tbody tr:last-child td{border-bottom:0}tbody tr:hover{background:#fafbff}.sku,.batch-id{font-weight:750;color:#273654}.product-name{font-weight:650;color:#27334a}.price{font-variant-numeric:tabular-nums;font-weight:650;color:#263650}.version{display:inline-flex;padding:3px 7px;background:#f0f3f9;border-radius:6px;font-size:10px;color:#59677e;font-weight:700}.status{display:inline-flex;align-items:center;gap:6px;padding:4px 8px;border-radius:20px;font-size:10px;font-weight:750}.status:before{content:"";width:5px;height:5px;border-radius:50%;background:currentColor}.status-completed,.status-active{background:#e8f8f0;color:#14764e}.status-pending{background:#fff4df;color:#99610a}.status-progress{background:#eaf0ff;color:#355dc5}.status-failed,.status-error{background:#ffedef;color:#b43442}.status-withdrawn{background:#f0f1f5;color:#737d8f}.status-live{background:#e8f8f0;color:#14764e}.empty{padding:30px 16px;text-align:center;color:var(--muted);font-size:12px}.table-foot{padding:12px 19px;border-top:1px solid var(--line);display:flex;justify-content:space-between;gap:10px;color:var(--muted);font-size:11px}.span-all{grid-column:1/-1}.batch-progress{display:inline-flex;align-items:center;gap:5px;font-variant-numeric:tabular-nums}.batch-error{max-width:230px;white-space:normal;color:var(--red);font-size:11px}.footer-note{text-align:center;color:#98a1b1;font-size:11px;padding:10px 0 0}.mobile-menu{display:none}.hidden{display:none!important}
@media(max-width:1150px){.sidebar{width:205px}.main{margin-left:205px}.topbar{padding:0 24px}.content{padding:26px 24px 36px}.metrics{grid-template-columns:repeat(2,minmax(0,1fr))}.section-grid{grid-template-columns:1fr}.import-panel{order:0}}
@media(max-width:700px){.sidebar{position:static;width:auto;height:auto;padding:12px 14px;flex-direction:row;align-items:center;justify-content:space-between}.brand{padding:0}.brand-mark{width:32px;height:32px}.sidebar nav,.nav-label,.side-bottom{display:none}.main{margin-left:0}.topbar{height:54px;padding:0 16px}.crumb{font-size:11px}.system-status{font-size:0;padding:8px}.system-status .online-dot{margin:0}.avatar{width:30px;height:30px}.content{padding:23px 14px 28px}.page-head{align-items:flex-start}.page-head h1{font-size:23px}.page-head .btn{padding:9px 10px;font-size:11px}.metrics{gap:10px}.metric{padding:14px}.metric-value{font-size:25px}.metric-top{font-size:11px;align-items:flex-start}.metric-icon{width:30px;height:30px}.panel-head{padding:16px}.panel-body{padding:16px}.table-tools{padding:12px;flex-wrap:wrap}.form-row{grid-template-columns:1fr}.form-row .btn{width:100%}.import-footer{align-items:flex-start;flex-direction:column}.import-footer .btn{width:100%}.table-foot{padding:11px 13px}.panel-title{font-size:14px}}
@media(prefers-reduced-motion:reduce){*{scroll-behavior:auto!important;transition:none!important}}
</style></head><body>
<div class="app">
<aside class="sidebar">
  <div class="brand"><div class="brand-mark">▦</div><div>Danh mục nhà cung cấp<small>Hệ thống quản lý</small></div></div>
  <nav aria-label="Điều hướng chính"><div class="nav-label">Không gian làm việc</div><a class="nav-link active" href="#overview"><span class="nav-icon">▦</span>Tổng quan</a><a class="nav-link" href="#products"><span class="nav-icon">▤</span>Sản phẩm</a><a class="nav-link" href="#batches"><span class="nav-icon">↻</span>Lịch sử nhập lô</a><div class="nav-label" style="margin-top:20px">Hệ thống</div><a class="nav-link" href="#import"><span class="nav-icon">＋</span>Nhập dữ liệu mới</a></nav>
  <div class="side-bottom"><div><span class="online-dot"></span>Dịch vụ nội bộ đã kết nối</div><div style="margin-top:8px">Dữ liệu danh mục được lưu trên máy</div></div>
</aside>
<main class="main">
  <header class="topbar"><div class="crumb">Không gian làm việc&nbsp; / &nbsp;<strong>Danh mục nhà cung cấp</strong></div><div class="top-right"><div class="system-status"><span class="online-dot"></span>API đã kết nối</div><div class="avatar" aria-label="Quản trị viên cục bộ">SC</div></div></header>
  <div class="content" id="overview">
    <div class="page-head"><div><p class="eyebrow">QUẢN LÝ NHÀ CUNG CẤP</p><h1>Tổng quan danh mục</h1><p class="subtitle">Quản lý cập nhật sản phẩm và theo dõi các lô dữ liệu nhập vào.</p></div><button class="btn btn-primary" id="jumpImport">＋ &nbsp;Nhập dữ liệu mới</button></div>
    <section class="metrics" aria-label="Tóm tắt danh mục">
      <article class="metric"><div class="metric-top"><span>Tổng số sản phẩm</span><span class="metric-icon">▤</span></div><div class="metric-value" id="metricProducts">—</div><div class="metric-note">Tất cả bản ghi trong danh mục</div></article>
      <article class="metric"><div class="metric-top"><span>Lô đã hoàn thành</span><span class="metric-icon">✓</span></div><div class="metric-value" id="metricCompleted">—</div><div class="metric-note">Đã xử lý xong</div></article>
      <article class="metric"><div class="metric-top"><span>Đang xử lý / Chờ xử lý</span><span class="metric-icon">◷</span></div><div class="metric-value" id="metricPending">—</div><div class="metric-note">Đang chờ hoặc đang xử lý</div></article>
      <article class="metric"><div class="metric-top"><span>Sản phẩm đã thu hồi</span><span class="metric-icon">⊘</span></div><div class="metric-value" id="metricWithdrawn">—</div><div class="metric-note">Vẫn lưu dấu vết trong danh mục</div></article>
    </section>
    <div class="section-grid">
      <section class="panel import-panel" id="import"><div class="panel-head"><div><h2 class="panel-title">Nhập một lô dữ liệu</h2><p class="panel-subtitle">Gửi thông tin cập nhật sản phẩm của nhà cung cấp theo định dạng JSON.</p></div><span class="status status-live">Sẵn sàng</span></div>
        <div class="panel-body"><form id="importForm"><div class="form-row"><div><label class="field-label" for="batchID">Mã lô</label><input class="text-input" id="batchID" name="batchID" value="batch-001" placeholder="Ví dụ: supplier-2026-001" required><p class="field-help">Dùng lại mã này khi thử gửi lại cùng một lô dữ liệu.</p></div><button class="btn btn-primary" id="importButton" type="submit">↥ &nbsp;Nhập lô</button></div>
          <div class="json-label-row"><label class="field-label" for="payload">Dữ liệu JSON</label><button class="text-button" id="resetSample" type="button">Khôi phục dữ liệu mẫu</button></div><textarea class="json-input" id="payload" spellcheck="false" required>[\n  {"sku":"SKU-100","name":"Desk Lamp","price":29.99,"version":1,"withdrawn":false},\n  {"sku":"SKU-200","name":"Office Chair","price":129.5,"version":1,"withdrawn":false}\n]</textarea><p class="field-help">Các trường bắt buộc: sku, name, price, version, withdrawn. Dung lượng tối đa: 1 MB.</p>
          <div id="message" class="message" role="status" aria-live="polite"></div><div class="import-footer"><span class="hint">Phiên bản mới hơn sẽ thay thế dữ liệu cũ. Phiên bản bằng hoặc cũ hơn sẽ bị bỏ qua.</span><button class="btn btn-primary" id="importButtonBottom" type="submit">Nhập sản phẩm &nbsp;→</button></div>
        </form></div>
      </section>
      <section class="panel" id="products"><div class="panel-head"><div><h2 class="panel-title">Danh mục sản phẩm</h2><p class="panel-subtitle">Các sản phẩm hiện có và trạng thái phiên bản.</p></div><button class="btn" id="refreshProducts">↻ &nbsp;Làm mới</button></div>
        <div class="table-tools"><label class="search-wrap"><span class="search-symbol">⌕</span><input class="search-input" id="productSearch" type="search" placeholder="Tìm theo SKU hoặc tên sản phẩm..." aria-label="Tìm sản phẩm"></label><select class="select-input" id="productFilter" aria-label="Lọc sản phẩm"><option value="all">Tất cả sản phẩm</option><option value="active">Chỉ đang hoạt động</option><option value="withdrawn">Chỉ đã thu hồi</option></select></div>
        <div class="table-wrap" id="productsTable"><div class="empty">Đang tải sản phẩm…</div></div><div class="table-foot"><span id="productCount">0 sản phẩm</span><span>Sắp xếp theo SKU</span></div>
      </section>
      <section class="panel span-all" id="batches"><div class="panel-head"><div><h2 class="panel-title">Lịch sử nhập lô</h2><p class="panel-subtitle">Theo dõi kết quả xử lý và các lô có thể tiếp tục xử lý.</p></div><button class="btn" id="refreshBatches">↻ &nbsp;Làm mới</button></div>
        <div class="table-tools"><label class="search-wrap"><span class="search-symbol">⌕</span><input class="search-input" id="batchSearch" type="search" placeholder="Tìm theo mã lô hoặc thông báo lỗi..." aria-label="Tìm lô nhập"></label><select class="select-input" id="batchFilter" aria-label="Lọc lô nhập"><option value="all">Tất cả trạng thái</option><option value="Completed">Hoàn thành</option><option value="Pending">Đang chờ</option><option value="In Progress">Đang xử lý</option><option value="Failed">Thất bại</option></select></div>
        <div class="table-wrap" id="batchesTable"><div class="empty">Đang tải các lô nhập…</div></div><div class="table-foot"><span id="batchCount">0 lô nhập</span><span>Mã lô được sắp xếp theo thứ tự chữ cái</span></div>
      </section>
    </div>
    <div class="footer-note">Dữ liệu được lưu trong catalog.json</div>
  </div>
</main></div>
<script>
const samplePayload = '[\\n  {"sku":"SKU-100","name":"Desk Lamp","price":29.99,"version":1,"withdrawn":false},\\n  {"sku":"SKU-200","name":"Office Chair","price":129.5,"version":1,"withdrawn":false}\\n]';
let allProducts = [], allBatches = [];
const $ = id => document.getElementById(id);
function node(tag, className, text) { const el = document.createElement(tag); if (className) el.className = className; if (text !== undefined && text !== null) el.textContent = String(text); return el; }
function cell(row, value, className) { const td = node('td', className || '', value === undefined || value === null ? '' : value); row.appendChild(td); return td; }
function badge(label, kind) { return node('span', 'status status-' + kind, label); }
function emptyState(message) { return node('div', 'empty', message); }
function formatPrice(value) { const n = Number(value); return Number.isFinite(n) ? n.toLocaleString(undefined, {minimumFractionDigits:2, maximumFractionDigits:2}) : '—'; }
function statusKind(status) { if (status === 'Completed') return 'completed'; if (status === 'Pending') return 'pending'; if (status === 'In Progress') return 'progress'; if (status === 'Failed' || status === 'Error') return 'failed'; return 'pending'; }
function translateStatus(status) { return ({'Completed':'Hoàn thành','Pending':'Đang chờ','In Progress':'Đang xử lý','Failed':'Thất bại','Error':'Lỗi'})[status] || status || 'Không rõ'; }
function translateError(message) { return String(message).replace('lô dữ liệu phải có ít nhất một bản ghi','lô dữ liệu phải có ít nhất một bản ghi').replace('sku is required','bắt buộc phải có SKU').replace('duplicate SKU','SKU bị trùng').replace('version must be >= 1','phiên bản phải lớn hơn hoặc bằng 1').replace('name is required','bắt buộc phải có tên sản phẩm').replace('price cannot be negative','giá không được âm').replace('bắt buộc phải có mã lô','bắt buộc phải có mã lô').replace('already exists with a different payload','đã tồn tại với dữ liệu khác').replace('invalid JSON:','JSON không hợp lệ:').replace('unexpected end of JSON input','JSON chưa đầy đủ hoặc thiếu dấu đóng'); }
function renderProducts() {
  const query = $('productSearch').value.trim().toLowerCase(), filter = $('productFilter').value;
  const rows = allProducts.filter(p => (filter === 'all' || (filter === 'withdrawn' ? p.withdrawn : !p.withdrawn)) && (String(p.sku || '').toLowerCase().includes(query) || String(p.name || '').toLowerCase().includes(query)));
  const host = $('productsTable'); host.replaceChildren();
  if (!rows.length) host.appendChild(emptyState(allProducts.length ? 'Không tìm thấy sản phẩm phù hợp.' : 'Chưa có sản phẩm. Hãy nhập một lô dữ liệu để bắt đầu.'));
  else { const table = node('table'), thead = node('thead'), hr = node('tr'); ['SKU','Tên sản phẩm','Giá','Phiên bản','Trạng thái'].forEach(h => hr.appendChild(node('th','',h))); thead.appendChild(hr); table.appendChild(thead); const tbody = node('tbody'); rows.forEach(p => { const tr = node('tr'); cell(tr,p.sku,'sku'); cell(tr,p.name,'product-name'); cell(tr,formatPrice(p.price),'price'); const v = cell(tr,''); v.appendChild(node('span','version','v'+p.version)); const s = cell(tr,''); s.appendChild(badge(p.withdrawn ? 'Đã thu hồi' : 'Đang hoạt động',p.withdrawn ? 'withdrawn' : 'active')); tbody.appendChild(tr); }); table.appendChild(tbody); host.appendChild(table); }
  $('productCount').textContent = rows.length + ' / ' + allProducts.length + ' sản phẩm';
}
function renderBatches() {
  const query = $('batchSearch').value.trim().toLowerCase(), filter = $('batchFilter').value;
  const rows = allBatches.filter(b => (filter === 'all' || b.status === filter) && (String(b.id || '').toLowerCase().includes(query) || String(b.error || '').toLowerCase().includes(query)));
  const host = $('batchesTable'); host.replaceChildren();
  if (!rows.length) host.appendChild(emptyState(allBatches.length ? 'Không tìm thấy lô nhập phù hợp.' : 'Chưa có lô nhập nào. Các lô bạn gửi sẽ hiển thị tại đây.'));
  else { const table = node('table'), thead = node('thead'), hr = node('tr'); ['Mã lô','Trạng thái','Đã cập nhật','Đã bỏ qua','Tiến độ','Chi tiết'].forEach(h => hr.appendChild(node('th','',h))); thead.appendChild(hr); table.appendChild(thead); const tbody = node('tbody'); rows.forEach(b => { const tr = node('tr'); cell(tr,b.id,'batch-id'); const s = cell(tr,''); s.appendChild(badge(translateStatus(b.status || 'Không rõ'),statusKind(b.status))); cell(tr,Number(b.processed || 0)); cell(tr,Number(b.skipped || 0)); const progress = cell(tr,''); progress.appendChild(node('span','batch-progress',Math.min(Number(b.nextIndex || 0), (b.records || []).length) + ' / ' + ((b.records || []).length))); const err = cell(tr,''); if (b.error) err.appendChild(node('span','batch-error',b.error)); else err.textContent = '—'; tbody.appendChild(tr); }); table.appendChild(tbody); host.appendChild(table); }
  $('batchCount').textContent = rows.length + ' / ' + allBatches.length + ' lô nhập';
}
function renderMetrics() { $('metricProducts').textContent = allProducts.length.toLocaleString(); $('metricCompleted').textContent = allBatches.filter(b => b.status === 'Completed').length.toLocaleString(); $('metricPending').textContent = allBatches.filter(b => b.status === 'Pending' || b.status === 'In Progress').length.toLocaleString(); $('metricWithdrawn').textContent = allProducts.filter(p => p.withdrawn).length.toLocaleString(); }
async function getJSON(url) { const response = await fetch(url); if (!response.ok) throw new Error('Yêu cầu thất bại (' + response.status + ')'); return response.json(); }
async function refresh() { try { const [products,batches] = await Promise.all([getJSON('/api/products'),getJSON('/api/batches')]); allProducts = Array.isArray(products) ? products : []; allBatches = Array.isArray(batches) ? batches : []; renderMetrics(); renderProducts(); renderBatches(); } catch (err) { showMessage('Không thể tải dữ liệu danh mục: ' + err.message, 'error'); } }
function showMessage(text, kind) { const msg = $('message'); msg.textContent = text; msg.className = 'message show ' + kind; }
async function importBatch(event) { event.preventDefault(); const buttons = [$('importButton'),$('importButtonBottom')]; let records; try { records = JSON.parse($('payload').value); if (!Array.isArray(records)) throw new Error('Dữ liệu JSON phải là một danh sách bản ghi.'); } catch (err) { showMessage('JSON không hợp lệ: ' + err.message, 'error'); return; } const id = $('batchID').value.trim(); if (!id) { showMessage('Vui lòng nhập mã lô.', 'error'); $('batchID').focus(); return; } buttons.forEach(b => { b.disabled = true; b.textContent = 'Đang nhập…'; }); showMessage('Đang gửi lô dữ liệu để xử lý…', 'info'); try { const response = await fetch('/api/import',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({batchId:id,records})}); let data; try { data = await response.json(); } catch (_) { throw new Error('Máy chủ trả về phản hồi không hợp lệ.'); } if (!response.ok) throw new Error(data.error || 'Nhập dữ liệu thất bại.'); showMessage('Lô ' + data.id + ' · ' + translateStatus(data.status) + ' · ' + data.processed + ' bản ghi đã cập nhật · ' + data.skipped + ' bản ghi bị bỏ qua.', 'success'); await refresh(); } catch (err) { showMessage('Nhập dữ liệu thất bại: ' + translateError(err.message), 'error'); } finally { buttons.forEach((b,i) => { b.disabled = false; b.textContent = i === 0 ? '↥  Nhập lô' : 'Nhập sản phẩm  →'; }); } }
$('importForm').addEventListener('submit',importBatch); $('productSearch').addEventListener('input',renderProducts); $('productFilter').addEventListener('change',renderProducts); $('batchSearch').addEventListener('input',renderBatches); $('batchFilter').addEventListener('change',renderBatches); $('refreshProducts').addEventListener('click',refresh); $('refreshBatches').addEventListener('click',refresh); $('jumpImport').addEventListener('click',() => { $('import').scrollIntoView({behavior:'smooth',block:'start'}); $('batchID').focus({preventScroll:true}); }); $('resetSample').addEventListener('click',() => { $('payload').value = samplePayload.replaceAll('\\n','\n'); showMessage('Đã khôi phục dữ liệu mẫu. Hãy đổi mã lô trước khi nhập lại với dữ liệu khác.', 'info'); });
// Render the starter payload with real newlines in the editor.
$('payload').value = samplePayload.replaceAll('\\n','\n');
refresh();
</script></body></html>`

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "phương thức không được hỗ trợ"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/api/products", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			return
		}
		writeJSON(w, 200, a.Products())
	})
	mux.HandleFunc("/api/batches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "method not allowed", 405)
			return
		}
		writeJSON(w, 200, a.Batches())
	})
	mux.HandleFunc("/api/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var req struct {
			BatchID string          `json:"batchId"`
			Records []ProductUpdate `json:"records"`
		}
		if err := decoder.Decode(&req); err != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "dữ liệu gửi lên vượt quá giới hạn 1 MiB"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON không hợp lệ: " + err.Error()})
			return
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "chỉ được gửi một đối tượng JSON"})
			return
		}
		b, err := a.Import(req.BatchID, req.Records)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, b)
	})
	return mux
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	dataPath := os.Getenv("CATALOG_DATA")
	if dataPath == "" {
		dataPath = filepath.Join("data", "catalog.json")
	}
	app, err := NewApp(dataPath)
	if err != nil {
		log.Fatal(err)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + strconv.Itoa(mustPort(port))
	log.Printf("Ứng dụng đang chạy tại http://localhost%s", addr)
	server := &http.Server{
		Addr:              addr,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}
func mustPort(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		log.Fatalf("PORT không hợp lệ %q", s)
	}
	return n
}