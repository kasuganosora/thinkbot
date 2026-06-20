package dao

// User 用户表（示例模型）。
type User struct {
	ID    uint   `gorm:"primaryKey"`
	Name  string `gorm:"size:255;not null"`
	Email string `gorm:"uniqueIndex;size:255;not null"`
}
