package models

// Project represents a managed project in the Lambs system.
type Project struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Repo           string      `json:"repo"`
	Desc           string      `json:"description"`
	IconURL        string      `json:"icon_url"`
	Stack          string      `json:"stack"`
	Port           string      `json:"port"`
	DB             string      `json:"db_type"`
	DSN            string      `json:"dsn"`
	UserCount      int         `json:"users_count"`
	Status         string      `json:"status"`
	Order          int         `json:"sort_order"`
	Pinned         bool        `json:"is_pinned"`
	IconCls        string      `json:"icon_cls"`
	BasePath       string      `json:"base_path"`
	BackendURL     string      `json:"backend_url"`
	ServiceName    string      `json:"service_name"`
	StartupCommand string      `json:"startup_command"`
	HealthURL      string      `json:"health_url"`
	Tags                interface{} `json:"tags"`
	OfflineMsg          string      `json:"offline_msg"`
	Features            interface{} `json:"features"`
	Tabs                interface{} `json:"tabs"`
	Datasources         interface{} `json:"datasources"`
	Services            interface{} `json:"services"`
	CreatedAt           string      `json:"created_at"`
	UpdatedAt           string      `json:"updated_at"`
	BackupIntervalHours int         `json:"backup_interval_hours"`
	BackupRetentionDays int         `json:"backup_retention_days"`
}

// User represents a Lambs system user.
type User struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	PasswordHash  string `json:"-"`
	Role          string `json:"role"`
	Status        string `json:"status"`
	ProjectAccess string `json:"project_access"`
	AvatarURL     string `json:"avatar_url"`
	LastLogin     string `json:"last_login"`
}

// Notification represents a system notification.
type Notification struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Read      bool   `json:"read"`
	CreatedAt string `json:"created_at"`
}

// AuditLog represents an audit trail entry.
type AuditLog struct {
	ID        int    `json:"id"`
	UserID    string `json:"user_id"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

// ApiResponse is the standard JSON response envelope.
type ApiResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Message string      `json:"message,omitempty"`
}

// Claims is the JWT claims structure.
type Claims struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

// Config holds server configuration (loaded from lambs_config.json).
type Config struct {
	JWTSecret      string `json:"jwt_secret"`
	AdminEmail     string `json:"admin_email"`
	Port           int    `json:"port"`
	RefreshInt     int    `json:"refresh_interval"`
	SMTPHost       string `json:"smtp_host"`
	SMTPPort       string `json:"smtp_port"`
	SMTPUser       string `json:"smtp_user"`
	SMTPPassword   string `json:"smtp_password"`
	SMTPFrom       string `json:"smtp_from"`
	RuntimeEnabled bool   `json:"runtime_enabled"`
	RuntimeBase    string `json:"runtime_base"`
}
