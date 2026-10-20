# TypeScript / JavaScript provider

Provider dùng TypeScript compatibility API đã pin @typescript/typescript6@6.0.2, compiler typescript@6.0.3 và Node 24.22.0. Descriptor công bố typescript, javascript, tsx, jsx, license Apache-2.0 và digest SHA-256 của manifest gồm lockfile, package metadata và mọi module trong src/.

## Phạm vi phân tích

Provider trả về descriptor, symbols, relations và diagnostics theo shared LanguageProvider contract. Symbol và relation có ID xác định từ path, byte range và identity; range là offset UTF-8 như Go contract yêu cầu.

Chế độ syntax giữ declarations và import/re-export đã khai báo, nhưng để target chưa được bind là null. Chế độ typed_when_available tạo TypeScript program trên captured files, dùng checker cho symbol identity, scopes, imports, named/star re-exports, path aliases và project references. Nếu một config hoặc source không có trong project đã resolve, provider giữ facts cú pháp và ghi gap thay vì nâng thành typed.

any, import(), CommonJS require, import không bind được và runtime JSX/framework semantics được ghi thành diagnostics/relations có trạng thái unknown. Đây không phải bằng chứng về hành vi runtime.

## Ranh giới dữ liệu và thực thi

Provider chỉ nhận các file có path tương đối, base64 và SHA-256 khớp. Tổng source tối đa 10 MiB, tối đa 4.096 files, 1 MiB path bytes, 64 config files trong đồ thị config. Duplicate path, traversal, file ngoài root, UTF-8 sai và profile không khớp toolchain bị từ chối.

tsconfig, extends và project references chỉ được chấp nhận khi file tương ứng nằm trong captured root. Compiler plugins bị từ chối trước khi gọi API phân tích project. Host ảo chỉ đọc captured files và .d.ts của compiler đã pin; emit bị tắt. Provider không chạy code, script, plugin, build, notebook hay network của target.

CLI đọc và ghi đúng một framed JSON message theo Content-Length, giới hạn frame 16 MiB và header 64 KiB. stdout chỉ chứa response frame; request lỗi trả mã thoát khác 0 và thông báo chung trên stderr.

## Chạy

Dùng Node 24.22.0 đã seal và npm 11.19.1:

    npm ci --ignore-scripts --no-audit --no-fund
    npm test
    npm start

npm start nhận một request frame qua stdin. Package tests bao phủ các ngôn ngữ được khai báo, identity same-name, re-export, aliases, project references, plugin rejection, dynamic gaps, byte ranges, profile/digest và framing.

Xem quyết định tương thích tại [adr/0001-typescript-api.md](adr/0001-typescript-api.md).
