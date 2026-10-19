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
