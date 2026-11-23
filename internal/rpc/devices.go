package rpc

type DeviceAddParams struct {
	User  string `json:"user"`
	Label string `json:"label"`
}

type DeviceListParams struct {
	User string `json:"user,omitempty"`
}

type DeviceRevokeParams struct {
	User  string `json:"user"`
	Label string `json:"label"`
	ID    string `json:"id,omitempty"`
}

type Device struct {
	ID        string `json:"id"`
	User      string `json:"user"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	Address   string `json:"address"`
	SetupCode string `json:"setup_code,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type DevicesReply struct {
	Devices []Device `json:"devices"`
}
