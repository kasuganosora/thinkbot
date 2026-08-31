// Package storage 提供 agent 模块的持久化层实现（SQLite/GORM）。
//
// 设计原则：
//   - 基础设施层：只依赖领域接口（memory.Repository），不反向依赖
//   - DDD 端口-适配器模式：本包是适配器，领域层定义端口（接口）
//   - 一个 DB 连接服务所有持久化需求，减少资源开销
//   - 所有 model 定义集中在 dao 包，通过 AutoMigrate 自动建表
//   - 对外暴露 NewXxxRepository 工厂函数，返回的类型实现领域接口
package storage
