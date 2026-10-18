# matrix summary (raw rows: matrix-results.json)

checks: 71 total, 0 failed; warnings: 0

| group | total | failed |
| --- | --- | --- |
| R | 26 | 0 |
| S | 10 | 0 |
| D | 12 | 0 |
| V | 16 | 0 |
| L | 1 | 0 |
| A | 5 | 0 |
| T | 1 | 0 |

| id | desc | result | detail |
| --- | --- | --- | --- |
| R/R0-mint | role tokens minted (publisher/approver/analyst/viewer) | PASS |  |
| R/R0b-collect | collect token minted for event seeding | PASS |  |
| R/R1-caps-publisher | capabilities map matches RBAC for publisher | PASS | 7 perms |
| R/R1-caps-approver | capabilities map matches RBAC for approver | PASS | 10 perms |
| R/R1-caps-analyst | capabilities map matches RBAC for analyst | PASS | 4 perms |
| R/R1-caps-viewer | capabilities map matches RBAC for viewer | PASS | 2 perms |
| R/R2-publisher-login | publisher signed in via console login page | PASS | http://127.0.0.1:28090 · 已登录 · mx-publisher |
| R/R2-publisher-save | publisher saved a draft via UI | PASS |  |
| R/R2-publisher-submit | publisher submitted draft for review via UI | PASS | flag mx-loop-67005 state=submitted |
| R/R3-approver-login | approver (second admin) signed in | PASS | http://127.0.0.1:28090 · 已登录 · mx-approver |
| R/R3-approver-sees-review | approver sees the submitted draft with approve action | PASS |  |
| R/R3-approver-approved | approver approved; publish action now primary | PASS |  |
| R/R3-published-rev1 | flag published at revision 1 after the loop | PASS | revision=1 |
| R/R3-audit-actors | audit attributes submit/approve/publish to two distinct admins | PASS | submit=mx-publisher (tok-7) approve=mx-approver (tok-8) publish=mx-approver (tok-8) |
| R/R4-analyst-read | analyst reads flag list (200) | PASS | HTTP 200 |
| R/R4-analyst-write-403 | analyst flag draft write rejected (403) | PASS | HTTP 403 |
| R/R4-analyst-submit-403 | analyst submit rejected (403) | PASS | HTTP 403 |
| R/R4-analyst-ui-gating | analyst UI hides the new-flag action | PASS |  |
| R/R4-analyst-detail-gating | analyst flag detail hides save/submit | PASS | save=0 submit=0 |
| R/R4-analyst-views | analyst analysis views render | PASS |  |
| R/R5-viewer-read | viewer reads flag list (200) | PASS | HTTP 200 |
| R/R5-viewer-write-403 | viewer flag write rejected (403) | PASS | HTTP 403 |
| R/R5-viewer-tokens-403 | viewer token management rejected (403) | PASS | HTTP 403 |
| R/R5-viewer-quota-403 | viewer quota write rejected (403) | PASS | HTTP 403 |
| R/R5-viewer-nav-gating | viewer sidebar hides tokens/quotas (permission-gated nav) | PASS | tokens=0 quotas=0 |
| R/R5-viewer-ui-gating | viewer UI hides new/save actions | PASS |  |
| S/S1-401-api | unauthenticated mutation → 401; anonymous read (signed-out) → 401 | PASS | POST=401 GET=401 |
| S/S1-401-ui | session-less browser is held at the login page | PASS |  |
| S/S3-409-api | stale expectedDraftHash write rejected with 409 | PASS | HTTP 409 |
| S/S3-409-ui | UI save after concurrent edit shows the 409 conflict panel (diff preserved) | PASS | otherSave=200 |
| S/S4-429-api | flag create at quota limit rejected with 429 quota_exceeded | PASS | HTTP 429 code=quota_exceeded |
| S/S4-429-ui | UI save over quota surfaces an error toast (no silent failure) | PASS |  |
| S/S5-offline | offline navigation to a cached route degrades to the in-page error banner | PASS | main=1 banner=1 |
| S/S5-offline-recovery | connectivity recovery restores the flags list | PASS | rows=8 |
| S/S5-offline-chunk | offline navigation to an uncached chunk keeps the shell and offers reload | PASS | shell=1 reload=1 |
| S/S5-offline-chunk-recovery | reload after connectivity returns renders the target page | PASS |  |
| D/D1-empty | empty workspace renders the empty state, not an error | PASS | links=0 empty="暂无开关 — 创建一个。" |
| D/D2-loading | loading state renders skeleton placeholders while real responses are in flight | PASS | skeletons=4 |
| D/D3-error | nonexistent flag renders the error state (fact → impact → action) | PASS | ← 功能开关 mx-missing-404 mx-missing-404 新建 编辑中草稿 待评审 已批准待发布 已发布  保存修改后，将草稿提交审核。  已登记的关联实验  本环境中此 Flag 关联实验共 0 个，当前展示 0 个；请根 |
| D/D4-zero-data-transition | seed experiment transitioned to running | PASS | before=scheduled transition=200 after=running |
| D/D4-zero-data-ui | zero-data running experiment shows its real state (no fabricated results) | PASS | ← 实验 exp-5 running flag u4-bool @ r2 · epoch 1 · 创建人 configured-admin-password running epoch 1 Flag  |
| D/D5-small-ingest | small-sample events ingested | PASS | {"accepted":55,"rejected":0} |
| D/D7-healthy-ingest | healthy-sample events ingested | PASS | {"accepted":308,"rejected":0} |
| D/D8-srm-ingest | SRM-skewed events ingested | PASS | {"accepted":242,"rejected":0} |
| D/D5-small-sample-ui | small sample renders the insufficient/watermark state (no fabricated significance) | PASS | ← 实验 exp-14 running flag mx-small @ r1 · epoch 1 · 创建人 configured-admin-password running epoch 1 Flag 版本 r1 注册版本 sha256: |
| D/D7-healthy-ui | healthy report renders real CI/sample-flow charts (SVG present) | PASS | svg=2 |
| D/D8-srm-ui | SRM-skewed allocation surfaces in the health zone | PASS | ← 实验 exp-16 running flag mx-srm @ r1 · epoch 1 · 创建人 configured-admin-password running epoch 1 Flag 版本 r1 注册版本 sha256:82 |
| D/D6-large-list | flags list renders with 1000+ drafts present (pagination intact) | PASS | total=1007 |
| V/V-390-overview | no horizontal overflow at 390px on overview | PASS | overflow=0px |
| V/V-390-flags | no horizontal overflow at 390px on flags | PASS | overflow=0px |
| V/V-390-flag-detail | no horizontal overflow at 390px on flag-detail | PASS | overflow=0px |
| V/V-390-exp-summary | no horizontal overflow at 390px on exp-summary | PASS | overflow=0px |
| V/V-768-overview | no horizontal overflow at 768px on overview | PASS | overflow=0px |
| V/V-768-flags | no horizontal overflow at 768px on flags | PASS | overflow=0px |
| V/V-768-flag-detail | no horizontal overflow at 768px on flag-detail | PASS | overflow=0px |
| V/V-768-exp-summary | no horizontal overflow at 768px on exp-summary | PASS | overflow=0px |
| V/V-1024-overview | no horizontal overflow at 1024px on overview | PASS | overflow=0px |
| V/V-1024-flags | no horizontal overflow at 1024px on flags | PASS | overflow=0px |
| V/V-1024-flag-detail | no horizontal overflow at 1024px on flag-detail | PASS | overflow=0px |
| V/V-1024-exp-summary | no horizontal overflow at 1024px on exp-summary | PASS | overflow=0px |
| V/V-1440-overview | no horizontal overflow at 1440px on overview | PASS | overflow=0px |
| V/V-1440-flags | no horizontal overflow at 1440px on flags | PASS | overflow=0px |
| V/V-1440-flag-detail | no horizontal overflow at 1440px on flag-detail | PASS | overflow=0px |
| V/V-1440-exp-summary | no horizontal overflow at 1440px on exp-summary | PASS | overflow=0px |
| L/L-combos | language × theme passes completed (4 combos × 6 key pages, screenshots + probes) | PASS | 24 screenshots |
| A/A1-zoom200-overview | 200% zoom equivalent (720 CSS px) keeps overview free of horizontal scroll | PASS | overflow=0px |
| A/A1-zoom200-flag-detail | 200% zoom equivalent (720 CSS px) keeps flag-detail free of horizontal scroll | PASS | overflow=0px |
| A/A2-reduced-motion | prefers-reduced-motion neutralizes the loading pulse animation | PASS | {"duration":"1e-05s","iterations":"1"} |
| A/A3-forced-colors | forced-colors mode keeps chart structure and textual status (no color-only information) | PASS | svg=2 |
| A/A4-contrast-more | prefers-contrast:more pass completed (screenshot + probes) | PASS |  |
| T/T-a11y-tree | a11y tree snapshots archived for the six key pages | PASS | a11y-tree/*.yaml |
