package game

type Move struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data,omitempty"`
}

type PublicState struct {
	// visible info per seat (filled by engine)
}

type Engine interface {
	Seats() int
	Join(userID string) (seat int, err error)
	StartIfReady() bool
	CurrentPlayer() int
	LegalMoves(seat int) []Move
	ApplyMove(seat int, m Move) (events []interface{}, err error)
	PublicState(viewSeat int) PublicState
	IsFinished() bool
}
