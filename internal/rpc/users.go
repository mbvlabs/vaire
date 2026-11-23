package rpc

type UserAddParams struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

type UserRevokeParams struct {
	User string `json:"user"`
}

type User struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type UsersReply struct {
	Users []User `json:"users"`
}
