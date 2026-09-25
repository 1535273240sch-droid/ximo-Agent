// Package storage 是 XimoAgent 的持久层门面（第9章）。
//
// 组成：
//   - sqlite 子包：连接管理（多读连接池 + 单写队列，第10章）；
//   - events.go：durable event log（run_events 表 + EventStore，第9.1章）；
//   - outbox.go：事务外发箱 + 后台 dispatcher（第25章）；
//   - migrations 子包：带版本号的 schema migration；
//   - repository 子包：各表的 CRUD repository；
//   - importer 子包：旧系统数据迁移（第30章）。
//
// 所有跨模块公共类型（Event / EventStore / OutboxStore / Tx）定义在本包，
// 供任务02（Engine）、任务04（工具运行时）直接复用。
package storage
