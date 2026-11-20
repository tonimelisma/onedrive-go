package syncfixture

import (
	"go/ast"
)

// FuncDeclName names a function as the exceptions do: Type.Method for a
// method, the name alone otherwise.
func FuncDeclName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	receiver := fn.Recv.List[0].Type
	if star, isStar := receiver.(*ast.StarExpr); isStar {
		receiver = star.X
	}
	if ident, isIdent := receiver.(*ast.Ident); isIdent {
		return ident.Name + "." + fn.Name.Name
	}

	return fn.Name.Name
}
