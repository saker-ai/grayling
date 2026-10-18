# keyboard walkthrough (raw: keyboard-walkthrough.json)

| id | check | result | detail |
| --- | --- | --- | --- |
| K1a | first Tab lands on skip-link | PASS | {"tag":"A","id":"","cls":"skip-link","role":"","text":"跳至主要内容","ariaCurrent":null,"ariaSelected":null,"tabindex":null} |
| K1b | skip-link activation moves focus to main | PASS | {"hash":"#/console/u4acc/prod/overview","activeId":"main","mainHasFocus":true,"mainVisible":true} |
| K1c | hash route survives skip-link | PASS | hash=#/console/u4acc/prod/overview |
| K1d | main content still rendered | PASS |  |
| K2a | every sidebar nav button is tabbable | PASS | 11 buttons |
| K2b | exactly one nav button carries aria-current=page | PASS | [{"text":"功能开关","tabbable":true,"ariaCurrent":"page","active":true}] |
| K2c | aria-current marks the flags view | PASS | 功能开关 |
| K2d | nav reachable in Tab sequence | PASS |  |
| K3a | ArrowRight moves focus to next tab | PASS | {"focused":"exp-tab-health","summarySelected":"true","hash":"#/console/u4acc/prod/experiments/exp-5"} |
| K3b | focus move does not select (manual activation) | PASS | summary aria-selected=true |
| K3c | focus move does not navigate | PASS | hash=#/console/u4acc/prod/experiments/exp-5 |
| K3d | Enter selects the focused tab | PASS | {"healthSelected":"true","hash":"#/console/u4acc/prod/experiments/exp-5/health","panelVisible":true} |
| K3e | selection deep-links the hash | PASS | hash=#/console/u4acc/prod/experiments/exp-5/health |
| K3f | tabpanel with matching id exists | PASS |  |
| K4a | dialog opens with focus inside | PASS |  |
| K4b | Tab cycles inside the dialog (trap) | PASS | cycle=INPUT. → INPUT. → BUTTON.btn → BUTTON.btn primary → INPUT. → INPUT. → BUTTON.btn → BUTTON.btn primary → INPUT. → INPUT. → BUTTON.btn → BUTTON.btn primary |
| K4c | focus never escapes to the page | PASS |  |
| K4d | Shift+Tab wraps from first to last | PASS | first=INPUT shiftTab→创建 |
| K4e | Escape closes the dialog | PASS |  |
| K4f | focus returns to the trigger | PASS | active=btn-new-ws |
| K5a | rollback confirm opens, reason field focused | PASS | active=action-reason |
| K5b | confirm button reachable by Tab | PASS | {"tag":"BUTTON","id":"","cls":"btn danger","role":"","text":"回滚到 r1","ariaCurrent":null,"ariaSelected":null,"tabindex":null} |
| K5c | rollback request completes and dialog closes | PASS | {"dialogOpen":false,"hash":"#/console/u4acc/prod/flags/u4-bool"} |
| K5d | pending flag.rollback approval created for r1 | PASS | id=approval-7 state=submitted payload={"revision":1} |
| K6 | decorative SVG charts never enter the Tab order | PASS | 28 stops walked |
| K7-light | focus ring screenshots (light) | PASS | PNG in evidence dir |
| K7-dark | focus ring screenshots (dark) | PASS | PNG in evidence dir |
