-- +goose Up
-- 待播预取和按热度常驻是两类需求，共享同一份预算，必须能分辨。
--
-- distribution_requests.pinned_until：这条请求当前紧迫（马上要放），影响认领顺序。
-- acceleration_objects.pinned_until：这个对象不可驱逐，GC 跳过它。
--
-- 两者都是 deadline 形状而非引用计数：房间游标停止推进、通知丢失或进程崩溃时，
-- 钉住会自行过期，不会永久占位。
ALTER TABLE distribution_requests ADD COLUMN pinned_until INTEGER NOT NULL DEFAULT 0;
ALTER TABLE acceleration_objects ADD COLUMN pinned_until INTEGER NOT NULL DEFAULT 0;

-- publish_on_cache_ready（"凡是进了本地缓存的都推一份"）把边缘的需求集合定义成了
-- 本地缓存的人口。本地预算 20 GiB、边缘预算 850 MiB 时，这等于让后者去镜像一个
-- 24 倍于自己的存储，必然抖动。改成显式的缓存模式：
--
--   prefetch          仅缓存队列视界内的曲目。工作集 = 房间数 × prefetch_horizon，
--                     有上界，不可能抖动；其余回源。此模式下待播可以用满预算。
--   prefetch_and_heat 视界 + 本地缓存就绪的曲目。视界优先且不可驱逐，但占用不超过
--                     prefetch_share_percent；剩下的份额归热度曲目。
ALTER TABLE accelerations ADD COLUMN cache_mode TEXT NOT NULL DEFAULT 'prefetch_and_heat'
    CHECK (cache_mode IN ('prefetch', 'prefetch_and_heat'));
ALTER TABLE accelerations ADD COLUMN prefetch_horizon INTEGER NOT NULL DEFAULT 2
    CHECK (prefetch_horizon >= 0);
-- 份额上限同时是热度曲目的保底份额：上限 20% 等价于热度保底 80%。它必须不大于
-- 低水位，否则 GC 的回收目标永远够不到——被钉住的部分它动不了。
ALTER TABLE accelerations ADD COLUMN prefetch_share_percent INTEGER NOT NULL DEFAULT 20
    CHECK (prefetch_share_percent BETWEEN 1 AND 100);
UPDATE accelerations SET cache_mode = 'prefetch' WHERE publish_on_cache_ready = 0;
ALTER TABLE accelerations DROP COLUMN publish_on_cache_ready;

-- +goose Down
ALTER TABLE accelerations ADD COLUMN publish_on_cache_ready INTEGER NOT NULL DEFAULT 0;
UPDATE accelerations SET publish_on_cache_ready = 1 WHERE cache_mode = 'prefetch_and_heat';
ALTER TABLE accelerations DROP COLUMN prefetch_share_percent;
ALTER TABLE accelerations DROP COLUMN prefetch_horizon;
ALTER TABLE accelerations DROP COLUMN cache_mode;
ALTER TABLE acceleration_objects DROP COLUMN pinned_until;
ALTER TABLE distribution_requests DROP COLUMN pinned_until;
