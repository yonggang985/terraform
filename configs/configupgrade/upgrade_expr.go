package configupgrade

import (
	"bytes"
	"fmt"

	hcl2 "github.com/hashicorp/hcl2/hcl"
	"github.com/hashicorp/terraform/tfdiags"

	hcl1ast "github.com/hashicorp/hcl/hcl/ast"
	hcl1printer "github.com/hashicorp/hcl/hcl/printer"
	hcl1token "github.com/hashicorp/hcl/hcl/token"

	"github.com/hashicorp/hil"
	hilast "github.com/hashicorp/hil/ast"
)

func upgradeExpr(val interface{}, filename string, interp bool) ([]byte, tfdiags.Diagnostics) {
	var buf bytes.Buffer
	var diags tfdiags.Diagnostics

	// "val" here can be either a hcl1ast.Node or a hilast.Node, since both
	// of these correspond to expressions in HCL2. Therefore we need to
	// comprehensively handle every possible HCL1 *and* HIL AST node type
	// and, at minimum, print it out as-is in HCL2 syntax.
	switch tv := val.(type) {

	case *hcl1ast.LiteralType:
		litVal := tv.Token.Value()
		switch tv.Token.Type {
		case hcl1token.STRING:
			if !interp {
				// Easy case, then.
				printQuotedString(&buf, litVal.(string))
				break
			}

			hilNode, err := hil.Parse(litVal.(string))
			if err != nil {
				diags = diags.Append(&hcl2.Diagnostic{
					Severity: hcl2.DiagError,
					Summary:  "Invalid interpolated string",
					Detail:   fmt.Sprintf("Interpolation parsing failed: %s", err),
					Subject:  hcl1PosRange(filename, tv.Pos()).Ptr(),
				})
			}

			interpSrc, interpDiags := upgradeExpr(hilNode, filename, interp)
			buf.Write(interpSrc)
			diags = diags.Append(interpDiags)

		case hcl1token.HEREDOC:
			// TODO: Implement
			panic("HEREDOC not supported yet")

		case hcl1token.BOOL:
			if litVal.(bool) {
				buf.WriteString("true")
			} else {
				buf.WriteString("false")
			}

		default:
			// For everything else (NUMBER, FLOAT) we'll just pass through the given bytes verbatim.
			buf.WriteString(tv.Token.Text)

		}

	case hcl1ast.Node:
		// If our more-specific cases above didn't match this then we'll
		// ask the hcl1printer package to print the expression out
		// itself, and assume it'll still be valid in HCL2.
		// (We should rarely end up here, since our cases above should
		// be comprehensive.)
		hcl1printer.Fprint(&buf, tv)

	case hilast.Node:
		// Nothing reasonable we can do here, so we should've handled all of
		// the possibilities above.
		panic(fmt.Errorf("upgradeExpr doesn't handle HIL node type %T", tv))

	default:
		// If we end up in here then the caller gave us something completely invalid.
		panic(fmt.Errorf("upgradeExpr on unsupported type %T", val))

	}

	return buf.Bytes(), diags
}
