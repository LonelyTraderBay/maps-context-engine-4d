# ADR 0001: Chọn API TypeScript cho provider

- Trạng thái: ACCEPTED_DELEGATED_AI theo BP-04/S53
- Task: T043, SPEC v1.6.13
- Phạm vi: discovery và profile provider TypeScript/JavaScript

## Bối cảnh

Workspace Explorer đang khóa TypeScript CLI `7.0.2`. Bản native CLI này không cung cấp compiler API ổn định để gọi từ Go hoặc Node. T043 cần compiler/language-service API có thể khóa phiên bản và license trước khi T044 xây binding thực.

## Quyết định

Dùng `@typescript/typescript6@6.0.2`, gói tương thích chính thức, với dependency alias khóa `typescript@6.0.3`. Cả hai gói khai báo Apache-2.0; lockfile ghi URL và integrity SHA-512. Giữ TypeScript CLI `7.0.2` trong workspace Explorer, không thay đổi dependency đó.

Provider khai báo TS, JS, TSX và JSX; mặc định `syntax`, tùy chọn `typed_when_available`; cấm project plugins. Fixture profile chỉ tham chiếu config thuộc package. Discovery không đọc config mục tiêu và không khởi chạy script, plugin hay build. T044 chịu trách nhiệm đặt compiler sau lớp xác thực root/capture và chỉ cấp dữ liệu đã kiểm soát.

## Fingerprint

- `compiler_digest`: SHA-256 của canonical JSON chứa đúng hai package đã resolve (`name`, `install_key`, `version`, `resolved`, `integrity`, `license`). Integrity SHA-512 ràng buộc nội dung tarball; digest này ràng buộc compiler compatibility wrapper cùng compiler thực.
- `provider_digest`: SHA-256 của canonical JSON manifest các file runtime provider (`package.json`, `package-lock.json`, `src/index.mjs`), sắp xếp theo raw UTF-8 path. Mỗi entry có path, byte length và SHA-256 của bytes. Test/docs không ảnh hưởng provider runtime identity.
- `node_digest`: SHA-256 của archive Node `24.22.0` đã seal tại T081.
- `profile-v1`: dùng `model.HashValue("profile-v1", effectiveProfile)` theo MCE4D C14N1 với đúng TSProfile S43; sort `tsconfig_paths`, reject duplicate paths, không thêm `profile_id` vào object.

Preimage của hai fingerprint là UTF-8 JSON không whitespace, key object sort theo UTF-8, array giữ thứ tự; SHA-256 trực tiếp trên bytes đó. Profile ID dùng riêng MCE4D C14N1 và domain separation qua `model.HashValue`. Artifact tại `artifacts/preflight/T043/` ghi preimage, thuật toán và kết quả. Nếu package, lock, provider runtime hoặc Node archive đổi thì phải tính lại digest và profile; không giữ hash cũ.

## Lựa chọn đã cân nhắc

1. Gọi TypeScript 7 CLI: bỏ vì không có API compiler ổn định và CLI không phải language-service binding.
2. Khóa TypeScript 6 compatibility package: chọn vì có API compiler/language-service, package chính thức, exact transitive resolution và license xác minh được.
3. Tự viết parser hoặc thêm parser framework: bỏ vì tăng code và không đáp ứng type/language-service contract.

## Ảnh hưởng và rollback

Provider API nằm trong package riêng, không đổi CLI Explorer. Nếu sau này TypeScript 7 cung cấp API chính thức đáp ứng cùng contract, so sánh API, license, lock integrity và fixtures bằng revision mới rồi thay pin; không đổi ngầm. Rollback hiện tại là bỏ package provider và profile fixture trước khi T044 phụ thuộc vào nó.

## Bằng chứng

`artifacts/preflight/T043/discovery-result.json` ghi package/version/license/integrity, API probe, local test và giới hạn xác minh. Discovery fixture không chứng minh phân tích project mục tiêu hoặc coverage semantic.
