package engine

import (
	"encoding/json"

	"github.com/jmal1/selfservice-api/internal/models"
)

func marshalActions(actions []models.Action) (json.RawMessage, error) {
	data, err := json.Marshal(actions)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(data), nil
}
