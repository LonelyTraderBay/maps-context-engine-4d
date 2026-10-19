# Fixture scale

`Plan` mô tả workload mục tiêu; `source_bytes_measured=false` và CLI luôn xuất
`execution_state=NOT_RUN`. Chỉ acceptance run mới có thể ghi metric đã đo.

Các cohort combined là S (1.000 file/20 MiB), M (10.000/200 MiB), L
(100.000/4 GiB) và XL (1.000.000/32 GiB). `count-only`, `byte-only` và
`combined` có oracle độc lập trong plan test.

Acceptance mặc định chạy S. Chọn cohort lớn hơn qua `MCE4D_SCALE_COHORT` và
`MCE4D_SCALE_MODE`; outer Go test timeout phải dài hơn deadline worker:

```powershell
$env:MCE4D_SCALE_COHORT = 'M'
go test -timeout=45m ./tests/acceptance -run '^TestScaleAcceptanceWorkload$' -count=1 -v
```

Với deadline hiện tại, dùng outer timeout ít nhất 2 giờ cho L. L sample trên
Windows/NTFS cần 912.9 giây cho 100.000 file/4 GiB; acceptance đặt ceiling
benchmark XL hữu hạn là 4 giờ và giữ sealed XL-V1 index deadline ở 2 giờ.
Trước XL dùng outer timeout ít nhất 5 giờ. Worker đặt timeout test riêng dài
hơn deadline xử lý 5 phút.

Worker chạy trong `internal/runtime/exec`: storage admission diễn ra trước khi
tạo dữ liệu và RSS được đo trên toàn process tree. Fixture dựng hai Git
worktree trên cùng volume, kiểm tra inventory và 10% membership churn, tạo
capture manifest, rồi đo database/WAL SQLite tổng hợp. SQLite fixture không
phải schema index sản phẩm. `SCALE_METRIC` chỉ mô tả workload đã chạy; metric
không chứng minh XL của sản phẩm đạt deadline hoặc capacity.

Trước cohort nặng, giữ lại kế hoạch gốc `NOT_RUN`, số đo host và quyết định
admission. Benchmark deadline sửa đổi chỉ dùng để hoàn tất phép đo synthetic và
so sánh với sealed target; không sửa sealed XL-V1 hay interactive defaults bằng
metric fixture.

## Kết quả đo XL tổng hợp

Windows 11 x64/NTFS, Go 1.27.2, 24 logical processors: fixture đã materialize
và đối chiếu đủ 1.000.000 file, 34.359.738.368 byte (32 GiB), với hai Git
worktree và churn 100.000 file thêm/xóa. Acceptance trả `PASS` sau 9.880.308 ms
(2 giờ 44 phút 40 giây); riêng oracle/hash độc lập mất 9.174.228 ms.

RSS đỉnh của process tree là 984.006.656 byte; capture manifest 179.900.446
byte; cache fixture đo được 478.901.294 byte; SQLite WAL fixture đạt đỉnh
149.943.312 byte. Dung lượng khả dụng giảm từ 542.109.077.504 xuống
493.300.916.224 byte; admission yêu cầu 49.442.488.832 byte và được chấp nhận.

Fixture vượt sealed XL-V1 index deadline 7.200 giây thêm 2.680.308 ms. Vì đây
là inventory/capture và SQLite facts fixture tổng hợp, kết quả chỉ chứng minh
phép thử này hoàn tất dưới benchmark ceiling 4 giờ; nó không chứng minh product
index đạt deadline hoặc xác nhận XL là envelope được hỗ trợ. Log và JSON metrics
được giữ trong evidence archive của T053.
