package query

import (
	"encoding/json"
	"fmt"
)

type CompiledExpression func(data interface{}) (interface{}, error)

func compileExpression(node Node) CompiledExpression {
	switch n := node.(type) {
	case *ExpressionNode:
		left := compileExpression(n.Left)
		right := compileExpression(n.Right)
		return func(data interface{}) (interface{}, error) {
			lval, err := left(data)
			if err != nil {
				return false, err
			}
			rval, err := right(data)
			if err != nil {
				return false, err
			}
			// Perform operation based on n.Operator
			return lval == rval, nil // Simplified for demonstration
		}
	case *IdentifierNode:
		return func(data interface{}) (interface{}, error) {
			// Access the field in data
			return nil, nil
		}
	case *ValueNode:
		return func(data interface{}) (interface{}, error) {
			return n.Value, nil
		}
	}
	return nil
}

func filter(record []byte, compiledExpr CompiledExpression) (bool, error) {
	var data interface{}
	err := json.Unmarshal(record, &data)
	if err != nil {
		return false, err
	}
	result, err := compiledExpr(data)
	if err != nil {
		return false, err
	}
	boolResult, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("expected boolean result, got %T", result)
	}
	return boolResult, nil
}
