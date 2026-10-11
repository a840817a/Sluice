# Ingest retry／drain 架構研究

研究基準：`00c572b`，2026-10-11。此輪只研究，沒有修改 runtime 行為。

結論：優先深化 queue 與 pipeline 交接時的「工作完成」責任，保留 Channel
現有的 VOD 三階段流程。尚未找到正常 VOD 路徑提早完成或死鎖的可重現案例。

## 現有契約

1. Source 以 `Push` 接收 live 工作，以 `PushPreserving` 接收有限來源工作。
   後者在容量滿時等待；已接收的 finite retry 允許暫時超出 pending 容量，
   避免唯一結果 consumer 等容量、worker 又等結果 channel 的循環等待。
2. `Broker.Pop` 在同一把鎖內移出 pending 並增加 outstanding。
3. fetch worker 送出結果後，完成責任交給 pipeline。送出前取消則由 worker Release。
4. pipeline 可能儲存、放棄、等 init 或重新排隊。重新排隊必須在 Release 前發生。
5. VOD transition 先 StopAdmission，等 Source 真正退出，再 AwaitDrain，最後 Join。
   Idle 僅表示 pending 與 outstanding 都為零；Join 才會等 init/key 工作與 goroutine 退出。
6. Stop 是取消並等待退出，並不保證完成所有已接收工作。取消後未消費的結果可能
   留下 outstanding，但舊 Runtime/Broker 被棄用；不能把 Stop 等同正常 drain。

主要證據：`internal/queue/broker.go`、`internal/fetch/fetcher.go`、
`internal/pipeline/init.go`、`internal/channel/ingest.go`、
`internal/channel/vod_transition.go`、`internal/channel/manager.go`。

## 發現

### 1. 完成責任的 interface 過於依賴呼叫順序（優先）

`Release()` 沒有 task 身分。fetch 有一個呼叫點，pipeline/init 有三個呼叫點。
呼叫端必須知道「恰好一次」與「Requeue 在 Release 前」兩個不變條件。
若兩個工作在途、第一個被誤 Release 兩次，Broker 就會提前 Idle；介面無法辨識。
這是可示範的介面風險，並非已發現 production 路徑重複 Release。

深化方向：由單一 module 管理每次取出工作的完成權與重試交接，讓跨 module
呼叫端不再自行維持計數／順序。這能提高 locality；同一 interface 的測試可
涵蓋成功、放棄、重試與取消，產生 leverage。尚不選定具體方法或型別。

### 2. Requeue 回傳值不能代表真的重新排隊（已確認）

`broker.go` live 分支呼叫無回傳值的 Push 後，直接回傳 true。若較高優先權的
工作佔滿 queue，retry 已被丟棄，Requeue 仍為 true。暫存 probe 實際輸出：
`Requeue=true, next=2, dropped=1`。

目前 Processor 忽略這個 bool，因此沒有證據顯示它造成現行 drain 錯誤。
但新呼叫端若以它判定是否續等，容易得到錯誤結論；應與完成契約一起處理。

### 3. Retry policy 分散，不能直接合併成一種重試（行為決策）

| 路徑 | 目前行為 |
| --- | --- |
| media HTTP 410 | 放棄，不 retry |
| media HTTP 425 | 500ms 起的指數退避，不輪替 fallback URL |
| 其他 media fetch 錯誤 | 2s 起的指數退避，輪替 fallback URL |
| media 寫檔失敗 | 1s 起的指數退避，重新下載 media |
| media validation／decrypt 失敗 | 放棄，不 retry |
| init | 獨立 budget、最多 1min backoff、後續較晚 live deadline 可重啟 |
| passthrough key warming | 15s timeout；失敗後後續 segment 可再 warm；VOD checkpoint 再嘗試 |

尤其 `Intake.decrypt` 的 key 網路取得失敗與無效 ciphertext 都變成 StageDecrypt，
且不 retry。這表示 key server 暫時失敗也可能失去已下載的 segment。這是程式
可直接確認的現有規則，本輪未做 origin 故障整合重現，不能宣稱是新回歸。
要不要重試、是否暫存 media、是否使用獨立 budget，應另做功能決策。

正常 config.Load 將 max_retries_static <= 0 正規化成 5；直接建構的零值 config
則在 media Broker 表示無限重試、init 表示 5 次。這是建構契約差異，不等於
正常啟動的 finite source 會無限 retry。

### 4. Dropped 不是所有失敗工作的總數（觀測語意）

Broker 計入容量淘汰、deadline 與 retry 耗盡；410、init 失敗、validation/decrypt
放棄則走 progress error，不全部計入 Dropped。不能用 Dropped 當單一完成結果。
現有 static shortfall 同時比較 stored 與 expected，並未只依賴 Dropped。

## 已有保障與缺口

已讀到針對 Idle 在途計數、Requeue 交接、finite 滿載 retry、手動 VOD 等待、
Stop 中斷 drain、key fetch 取消、VOD 等 key、init 取消／deadline 的測試。
這些測試支撐現有流程，沒有理由重寫整個 Channel lifecycle。

下一輪應補的測試：

- 同一工作的完成只能結算一次，不能替另一個工作減少 outstanding。
- 重試交接前後永遠不能短暫 Idle；滿載被淘汰的 retry 有明確結果。
- 取消發生在 fetch、結果 channel、init 暫存三個位置时，完成責任不重複交接。
- 正常 drain 與強制 Stop 使用各自正確的完成條件。

## 建議實作順序

1. 固定上述 contract 測試，保留現有 retry 次數、deadline 與容量規則。
2. 集中每次取出工作的完成／retry 交接責任，移除匿名 Release 的跨 module 配對。
3. 讓 queue 回報確切 retry 結果，再整理各類失敗的觀測語意。
4. 另行決定 key 暫時失敗的恢復策略；不混入保持行為的重構。

不建議先抽通用 Retry framework、合併 init/media budget，或以單一 Idle 取代 Join。
既有 ingestControl 是有 depth 的 module；刪掉它只會把順序知識散回 Channel callers。

## 驗證範圍

在 `/tmp/sluice-retry-study` 複製 queue 原始碼與既有測試，以隔離 Go cache 執行
queue 全套測試及兩個診斷 probe，全部通過。Probe 是觀察現有行為，不是新增的
regression guard。log：`/tmp/sluice-retry-study/results.txt`。

本輪沒有執行完整 make verify，沒有修改任何 Go 檔，也未做 production 故障實驗。

## 實作：每次 Pop 的完成責任

研究後採用 `queue.Attempt`：每次 `Broker.Pop` 建立独立完成狀態，與 task 一起
通過 fetch result 傳給 pipeline。`Finish` 結算一次；`Retry` 在同一把 Broker 鎖
內重新排隊並結算舊 attempt。兩者互斥，重複呼叫或複製 handle 都不會影響其他
工作；先前 attempt 也不能結算下一次 retry。舊的匿名 `Broker.Release` 與
`Broker.Requeue` 已移除。

`Retry` 的 true 表示邏輯工作已在 queue 中（包含與同 key 的 pending 工作合併），
false 表示已放棄或此 attempt 已結算。live 容量拒絕會回傳 false。重試次數、
退避、deadline、fallback URL 輪替、finite overflow 與解密失敗規則沒有改動。

完成後 task 的所有權也已交出，呼叫端不得再修改它；成功 Retry 後 worker 可以
立即取走同一 task。完成 handle 自身仍可重複 Finish，結果為 false。

Pool 送出結果前取消，由 worker 結算；結果成功送出後，由 consumer 結算。
init 暫存取消也由 consumer 結算。強制 Stop 的結果 channel 中尚未消費工作仍
隨舊 Runtime 棄用；這次沒有將 Stop 改為正常 drain，也沒有變更 Channel 的
StopAdmission／AwaitDrain／Join 順序。
