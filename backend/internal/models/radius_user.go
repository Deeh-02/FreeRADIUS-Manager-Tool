package models

import "time"

// RadiusUser represents a RADIUS network-access user.
type RadiusUser struct {
	ID                  int        `json:"id"`
	Username            string     `json:"username"`
	Password            string     `json:"-"`
	Email               string     `json:"email"`
	FullName            string     `json:"full_name"`
	Department          string     `json:"department"`
	Phone               string     `json:"phone"`
	Notes               string     `json:"notes"`
	Status              string     `json:"status"`
	DeviceLimit         int        `json:"device_limit"`
	AccountExpiry       *time.Time `json:"account_expiry"`
	PasswordExpiry      *time.Time `json:"password_expiry"`
	ForcePasswordChange bool       `json:"force_password_change"`
	PlanID              *int       `json:"plan_id"`
	PlanName            *string    `json:"plan_name"`
	PlanPrice           *float64   `json:"plan_price"`
	PlanCurrency        *string    `json:"plan_currency"`
	OpenInvoices        int        `json:"open_invoices"`
	CreatedBy           *int       `json:"created_by"`
	CreatedByUsername   string     `json:"created_by_username,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	ActiveSessions      int        `json:"active_sessions"`
}

// CreateRadiusUserRequest is the request body for creating a RADIUS user.
// Usernames may not contain spaces or quotes: they are used as RADIUS User-Name and in the
// SQL that FreeRADIUS runs (which escapes them, but routers handle them inconsistently).
type CreateRadiusUserRequest struct {
	Username            string  `json:"username" binding:"required,min=2,max=64,excludesall= '\"\\"`
	Password            string  `json:"password" binding:"required,min=6,max=128"`
	Email               string  `json:"email" binding:"omitempty,email"`
	FullName            string  `json:"full_name"`
	Department          string  `json:"department"`
	Phone               string  `json:"phone" binding:"max=30"`
	Notes               string  `json:"notes"`
	DeviceLimit         int     `json:"device_limit" binding:"omitempty,min=1,max=500"`
	AccountExpiry       *string `json:"account_expiry"`
	ForcePasswordChange bool    `json:"force_password_change"`
	PlanID              *int    `json:"plan_id"`
	StartNow            bool    `json:"start_now"`
	PromoCode           string  `json:"promo_code"`
}

// UpdateRadiusUserRequest is the request body for updating a RADIUS user. Omitted fields are
// unchanged; account_expiry "" clears the expiry (never expires).
type UpdateRadiusUserRequest struct {
	Email               *string `json:"email"`
	FullName            *string `json:"full_name"`
	Department          *string `json:"department"`
	Phone               *string `json:"phone" binding:"omitempty,max=30"`
	Notes               *string `json:"notes"`
	DeviceLimit         *int    `json:"device_limit" binding:"omitempty,min=1,max=500"`
	AccountExpiry       *string `json:"account_expiry"`
	ForcePasswordChange *bool   `json:"force_password_change"`
}

// ResetPasswordRequest is used to reset a RADIUS user's password.
type ResetPasswordRequest struct {
	NewPassword string `json:"new_password" binding:"required,min=6,max=128"`
}

// RadiusUserSession represents an active session for a RADIUS user.
type RadiusUserSession struct {
	SessionID       string    `json:"session_id"`
	Username        string    `json:"username"`
	NASIPAddress    string    `json:"nas_ip"`
	NASPortID       string    `json:"nas_port"`
	FramedIPAddress string    `json:"framed_ip"`
	CallingStation  string    `json:"calling_station"`
	CalledStation   string    `json:"called_station"`
	StartTime       time.Time `json:"start_time"`
	SessionDuration int64     `json:"session_duration"`
	InputOctets     int64     `json:"input_octets"`
	OutputOctets    int64     `json:"output_octets"`
}
