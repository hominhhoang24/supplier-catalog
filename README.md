# Supplier Catalog – Quản lý danh mục nhà cung cấp

Ứng dụng web viết bằng Go để nhập các đợt cập nhật sản phẩm từ nhà cung cấp, kiểm tra thứ tự phiên bản và theo dõi trạng thái từng batch. Dữ liệu được lưu cục bộ tại `data/catalog.json`.

## 1. Yêu cầu

- Go 1.23 trở lên
- Git (nếu cần đưa project lên GitHub)

## 2. Chạy ứng dụng trên Windows

Mở PowerShell hoặc Terminal trong VS Code, chuyển đến thư mục chứa `go.mod`, rồi chạy:

```powershell
go test -count=1 ./...
go vet ./...
go build ./...
go run .
```

Mở trình duyệt tại <http://localhost:8080>.

Mặc định ứng dụng chạy ở cổng `8080`. Có thể đổi cổng và đường dẫn file dữ liệu bằng biến môi trường:

```powershell
$env:PORT = "8081"
$env:CATALOG_DATA = ".\data\catalog.json"
go run .
```

## 3. Giao diện

Dashboard tiếng Việt hỗ trợ:

- Xem số lượng sản phẩm và trạng thái sản phẩm đã ngừng kinh doanh.
- Nhập batch sản phẩm bằng JSON.
- Tìm kiếm sản phẩm và lọc trạng thái.
- Theo dõi lịch sử batch, tiến độ xử lý và thông tin lỗi.
- Làm mới dữ liệu hiển thị.

Giao diện và API chạy cùng một tiến trình Go, không cần cài đặt hoặc build frontend riêng.

## 4. API

### `GET /api/health`

Kiểm tra tình trạng dịch vụ. Chỉ chấp nhận phương thức `GET`.

### `GET /api/products`

Trả về danh sách sản phẩm.

### `GET /api/batches`

Trả về danh sách batch đã nhập.

### `POST /api/import`

Nhận một đối tượng JSON gồm `batchId` và mảng `records` không rỗng. Ví dụ:

```json
{
  "batchId": "batch-001",
  "records": [
    {"sku":"SKU-100","name":"Desk Lamp","price":29.99,"version":1,"withdrawn":false},
    {"sku":"SKU-200","name":"Office Chair","price":129.5,"version":1,"withdrawn":false}
  ]
}
```

Mỗi bản ghi gồm:

- `sku`: mã sản phẩm bắt buộc; khoảng trắng đầu và cuối được loại bỏ.
- `name`: tên sản phẩm, bắt buộc nếu sản phẩm đang hoạt động.
- `price`: giá không được âm.
- `version`: số nguyên từ 1 trở lên.
- `withdrawn`: `true` nếu sản phẩm đã ngừng kinh doanh, ngược lại là `false`.

API từ chối JSON không hợp lệ, trường không được định nghĩa, nhiều đối tượng JSON nối tiếp nhau, phương thức HTTP không được hỗ trợ và request body lớn hơn 1 MiB.

## 5. Quy tắc xử lý batch

- Mã batch là bất biến. Gửi lại cùng mã batch với cùng dữ liệu là thao tác idempotent; dùng lại mã đó với dữ liệu khác sẽ bị từ chối.
- Với mỗi SKU, chỉ phiên bản lớn hơn phiên bản đang lưu mới thay thế dữ liệu hiện tại. Phiên bản bằng hoặc thấp hơn sẽ bị bỏ qua.
- Sản phẩm đã ngừng kinh doanh vẫn được lưu lại để giữ dấu vết. Phiên bản cao hơn có thể kích hoạt lại sản phẩm bằng cách đặt `withdrawn` thành `false`.
- Tiến độ batch được lưu sau mỗi bản ghi. Nếu ứng dụng dừng giữa chừng, batch có thể tiếp tục từ checkpoint đã lưu khi gửi lại cùng mã batch và dữ liệu.

## 6. Dữ liệu và giới hạn

Dữ liệu sản phẩm và tiến độ batch được lưu trong `data/catalog.json`. Ứng dụng ghi qua file tạm rồi đổi tên để giảm nguy cơ file bị ghi dở. Đây là demo cục bộ, một tiến trình; không hỗ trợ nhiều instance ghi đồng thời. Nếu triển khai thực tế, nên cân nhắc SQLite hoặc PostgreSQL, giao dịch, ràng buộc dữ liệu, xác thực và giám sát vận hành.

**Lưu ý:** Không xóa hoặc thay thế `data/catalog.json` nếu muốn giữ dữ liệu hiện có. Trước khi thử nghiệm, hãy sao lưu file này.

## 7. Chạy test

```powershell
go test -count=1 ./...
go vet ./...
go test -race -count=1 ./...
```

Các test bao gồm nhập idempotent, thứ tự phiên bản, sản phẩm đã ngừng kinh doanh, phục hồi sau khởi động lại, tái sử dụng mã batch, kiểm tra dữ liệu đầu vào và kiểm tra request HTTP.