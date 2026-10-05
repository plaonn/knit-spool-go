# Contributing and maintenance

## English

### Branch, pull request, CI, merge

Create a topic branch from `main` and open a pull request targeting `main`. Keep the change focused, explain its effect, and include relevant validation. Draft pull requests are welcome while work is incomplete.

All four existing GitHub Actions checks must pass before merging:

- `Go tests and static checks`
- `Linux amd64 cgo-free build`
- `Linux arm cgo-free build`
- `Official protocol conformance and Kotlin differential`

See [the workflow](.github/workflows/ci.yml) for the commands and pinned protocol reference. Run `go test ./...` and `go vet ./...` from the repository root for Go changes; reference checks are described in [README.md](README.md#conformance-and-ci). Documentation changes also go through the four CI checks.

The main ruleset requires a pull request with **zero required approving reviews**, so the solo maintainer can merge their own PR after checking the final diff and CI results. Force pushes and deletion of `main` are blocked, with no bypass actors configured. Requiring the branch to be up to date with the base is not enabled. Resolve conflicts and rerun relevant validation when incorporating newer changes; do not disable protection to get a change through.

### Versions, tags, and releases

Use `vMAJOR.MINOR.PATCH` tags. While the project is pre-1.0, use `v0.MINOR.PATCH`: patch releases for compatible fixes, minor releases for features or breaking changes, and explicitly describe every breaking change. Use a suffix such as `-rc.1` for a prerelease. A release version is separate from the wire protocol version.

Releases are a maintainer action after the selected commit has landed on `main` and its four checks have passed. Create an annotated tag at that exact commit and a matching GitHub Release; do not move or reuse published tags. Include the commit, protocol/reference revision, changes, configuration or storage compatibility, and upgrade/rollback notes. If attaching binaries, identify OS/architecture (including `GOARM=6` for ARMv6), build inputs, and SHA-256 checksums. Preserve [LICENSE](LICENSE) and make the corresponding source available.

There is no release automation in the current workflow. No tags or releases existed when this guide was prepared on 2026-10-05; until one is published, identify a build by its full commit SHA rather than an assumed version.

### Upgrade and rollback

Follow [README operations](README.md#operations). Before upgrading, record the current commit, binary and configuration; stop the service and take a consistent private backup of the SQLite store, retaining any WAL files present. Keep the previous binary and backup. Validate the new configuration with `knit-spool check` using the service environment, then replace the binary and start the service. Check `/healthz`, `/source`, logs, and client reconnection plus send/retrieve before accepting the upgrade. Keep credentials and production data out of PRs and logs.

If validation fails, stop the service and preserve the failed state before restoring the previous binary and configuration. Confirm that the older binary can read the current database; if compatibility is uncertain, restore the pre-upgrade backup instead. Restoring that backup loses writes received since it was taken, so make that consequence explicit. Repeat health and client checks after rollback. The [SQLite durability section](README.md#sqlite-durability) describes the separate `FULL`/`NORMAL` setting; changing it back does not recover lost writes.

### Security reports

Follow [SECURITY.md](SECURITY.md). Check the repository's [Security page](https://github.com/plaonn/knit-spool-go/security) for GitHub's private **Report a vulnerability** option. Use it only when available. On 2026-10-05, private vulnerability reporting was disabled, and no alternative private reporting address was verified. A private report route therefore remains to be established by the maintainer.

If that option is unavailable, ask the maintainer how to arrange a private channel without posting vulnerability details. Public issues, PRs, and profile pages are not private reporting channels. Never include live credentials, invites, user data, or production databases. Enabling a reporting feature or publishing a contact address is a separate maintainer decision.

## 한국어

### 브랜치, PR, CI, 병합

`main`에서 작업 브랜치를 만들고 `main` 대상 PR을 엽니다. 변경 목적과 영향을 설명하고 관련 검증 결과를 남깁니다. 작업 중에는 초안 PR을 사용할 수 있습니다.

위에 나열한 기존 GitHub Actions 검사 4개가 모두 통과해야 병합할 수 있습니다. 명령과 고정된 프로토콜 참조는 [workflow](.github/workflows/ci.yml)를 확인하세요. Go 변경은 저장소 루트에서 `go test ./...`, `go vet ./...`를 실행하고, 참조 검증은 [README](README.md#conformance-and-ci)를 따릅니다. 문서 변경도 같은 CI 4개를 거칩니다.

main 보호 규칙은 PR을 요구하며 **필수 승인 리뷰 수는 0**입니다. 1인 관리자는 최종 diff와 CI 결과를 확인한 뒤 자신의 PR을 병합할 수 있습니다. main의 강제 push와 삭제는 차단되며 우회 대상은 없습니다. base 최신 상태를 강제하는 옵션은 사용하지 않습니다. 새 변경을 반영할 때 충돌을 해결하고 관련 검증을 다시 수행하며, 병합을 위해 보호 규칙을 해제하지 않습니다.

### 버전, 태그, 릴리스

태그는 `vMAJOR.MINOR.PATCH`를 사용합니다. 1.0 이전에는 `v0.MINOR.PATCH`로 표기하고, 호환되는 수정은 patch, 기능 추가나 호환성 변경은 minor로 올립니다. 호환성을 깨는 변경은 반드시 설명합니다. 사전 릴리스에는 `-rc.1` 같은 접미사를 사용합니다. 릴리스 버전과 전송 프로토콜 버전은 별개입니다.

관리자는 대상 커밋이 main에 병합되고 CI 4개를 통과한 뒤 해당 커밋에 annotated tag와 같은 이름의 GitHub Release를 만듭니다. 게시한 태그를 이동하거나 재사용하지 않습니다. 릴리스에는 커밋, 프로토콜/참조 리비전, 변경 사항, 설정·저장소 호환성, 업그레이드·롤백 절차를 기록합니다. 바이너리를 첨부한다면 OS/아키텍처(ARMv6은 `GOARM=6` 포함), 빌드 입력과 SHA-256 체크섬을 명시합니다. [LICENSE](LICENSE)를 보존하고 대응 소스를 제공합니다.

현재 workflow에는 릴리스 자동화가 없습니다. 이 문서를 준비한 2026-10-05에는 태그와 릴리스가 없었습니다. 첫 릴리스가 게시되기 전에는 추정 버전 대신 전체 커밋 SHA로 빌드를 식별합니다.

### 업그레이드와 롤백

[README 운영 절차](README.md#operations)를 따릅니다. 현재 커밋·바이너리·설정을 기록하고 서비스를 중지한 뒤, 남아 있는 WAL 파일을 포함해 SQLite 저장소의 일관된 비공개 백업을 만듭니다. 이전 바이너리와 백업을 보관합니다. 서비스와 같은 환경으로 `knit-spool check`를 실행한 뒤 바이너리를 교체하고 시작합니다. `/healthz`, `/source`, 로그, 클라이언트 재연결과 송수신을 확인합니다. 자격 증명과 운영 데이터는 PR이나 로그에 넣지 않습니다.

검증에 실패하면 서비스를 중지하고 실패 당시 상태를 보존한 뒤 이전 바이너리·설정을 복원합니다. 이전 바이너리가 현재 DB를 읽을 수 있는지 확인하고, 호환성이 불확실하면 업그레이드 전 백업을 복원합니다. 백업 이후 수신한 쓰기가 사라진다는 점을 명확히 하고, 복원 뒤에도 상태·클라이언트 검증을 반복합니다. `FULL`/`NORMAL` 설정은 별도 [SQLite 내구성 안내](README.md#sqlite-durability)를 따르며, 설정 복원으로 이미 잃은 쓰기가 돌아오지는 않습니다.

### 보안 제보

[SECURITY.md](SECURITY.md)를 따릅니다. 저장소 [Security 페이지](https://github.com/plaonn/knit-spool-go/security)에 비공개 **Report a vulnerability** 항목이 있을 때만 해당 경로를 사용합니다. 2026-10-05 확인 당시 비공개 제보 기능은 꺼져 있었고, 대체 비공개 연락처도 확인되지 않았습니다. 관리자가 비공개 제보 경로를 마련해야 하는 상태입니다.

해당 항목이 없다면 취약점 세부 내용을 공개하지 않고 관리자에게 비공개 연락 경로를 문의하세요. 공개 이슈·PR·프로필 페이지는 비공개 제보 채널이 아닙니다. 실제 자격 증명, 초대 정보, 사용자 데이터, 운영 DB를 첨부하지 마세요. 제보 기능 활성화나 연락처 공개는 관리자의 별도 결정입니다.
