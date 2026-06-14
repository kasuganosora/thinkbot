package db

// User is a sample database model for the project.
type User struct {
	ID    uint   `gorm:"primaryKey"`
	Name  string `gorm:"size:255;not null"`
	Email string `gorm:"uniqueIndex;size:255;not null"`
}
