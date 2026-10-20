# TypeScript / JavaScript provider discovery

Gói này khóa API tương thích TypeScript 6 cho các bước phân tích do MCE4D quản lý. Workspace Explorer tiếp tục dùng TypeScript CLI 7; hai phạm vi có lockfile độc lập.

`@typescript/typescript6@6.0.2` là lớp tương thích chính thức. Lockfile cố định compiler thực tế `typescript@6.0.3` và integrity của cả hai tarball. API mặc định chỉ cho chế độ `syntax`; chế độ typed được bật có điều kiện ở bước tích hợp sau.

`fixtures/profile.json` là EffectiveProfile fixture theo S43, chỉ trỏ tới `fixtures/tsconfig.json` nằm trong package. Nó không mô tả một repository mục tiêu. Không nạp tsconfig mục tiêu, plugin, build hay script trong bước discovery này. T044 phải giới hạn config vào root đã capture và từ chối plugin trước khi gọi các API có thể phân giải project.

Chạy kiểm thử bằng Node `24.22.0` đã seal:

```sh
npm ci --ignore-scripts --no-audit --no-fund
npm test
```

Xem quyết định và cách tạo fingerprint tại [`adr/0001-typescript-api.md`](adr/0001-typescript-api.md).
