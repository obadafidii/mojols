; Mojo self / Self (highlighted before the general naming-convention
; rules below so they take precedence on the literal identifiers).

(identifier) @variable

((identifier) @variable.builtin
 (#eq? @variable.builtin "self"))

((identifier) @type.builtin
 (#eq? @type.builtin "Self"))

; Identifier naming conventions

((identifier) @type.builtin
 (#match? @type.builtin "^(AnyType|Arc|Array|Atomic|BFloat16|Bool|Byte|CollectionElement|CollectionElementNew|Comparable|Copyable|Defaultable|Deinitable|DType|Dict|DynamicVector|EqualityComparable|Error|ExplicitlyCopyable|FalseType|Float16|Float32|Float64|Float8|FloatLiteral|Hashable|Identifiable|ImmutableOrigin|ImplicitlyCopyable|Index|InlineArray|Int|Int8|Int16|Int32|Int64|IntLiteral|KeyElement|List|Movable|MutableOrigin|MutUntrackedOrigin|NoneType|Object|OpaquePointer|Optional|Pointer|PyObjectPtr|Python|PythonObject|Representable|Scalar|Self|Set|SIMD|Span|StaticTuple|String|StringLiteral|StringSlice|Tuple|UInt|UInt8|UInt16|UInt32|UInt64|UnsafePointer|Variant|VariadicList|VariadicPack|Writable)$"))

((identifier) @constructor
 (#match? @constructor "^[A-Z]"))

((identifier) @constant
 (#match? @constant "^[A-Z][A-Z_]*$"))

; Builtin functions

; Audited against Mojo stdlib (Mojo 1.0 / 1.1).
((call
  function: (identifier) @function.builtin)
 (#match?
   @function.builtin
   "^(abort|abs|align_of|all|any|ascii|atof|atol|bin|bitcast|breakpoint|chr|conforms_to|constrained|debug_assert|divmod|enumerate|external_call|global_constant|has_trait|hash|hex|input|iter|len|map|materialize|max|min|next|oct|open|ord|parallelize|partition|pow|print|range|rebind|rebind_var|reflect|repr|reversed|round|simd_width|size_of|slice|sort|swap|trait_downcast|trait_downcast_var|type_of|unroll|unsafe_bitcast|unsafe_memcpy|vectorize|zip)$"))

; Mojo built-in decorators (recognized before the generic @function below)

((decorator
  (identifier) @attribute.builtin)
 (#match? @attribute.builtin "^(always_inline|export|fieldwise_init|implicit|noinline|nonmaterializable|parameter|pure|register_passable|staticmethod|unroll|value)$"))

((decorator
  (call function: (identifier) @attribute.builtin))
 (#match? @attribute.builtin "^(always_inline|export|fieldwise_init|implicit|noinline|nonmaterializable|parameter|pure|register_passable|staticmethod|unroll|value)$"))

; Function calls

(decorator) @function

(call
  function: (attribute attribute: (identifier) @function.method))
(call
  function: (identifier) @function)

; Function definitions

(function_definition
  name: (identifier) @function)

(attribute attribute: (identifier) @property)
(type (identifier) @type)

; Literals

[
  (none)
  (true)
  (false)
] @constant.builtin

[
  (integer)
  (float)
] @number

(comment) @comment
(string) @string
(escape_sequence) @escape

(interpolation
  "{" @punctuation.special
  "}" @punctuation.special) @embedded

[
  "-"
  "-="
  "!="
  "*"
  "**"
  "**="
  "*="
  "/"
  "//"
  "//="
  "/="
  "&"
  "%"
  "%="
  "^"
  "+"
  "->"
  "+="
  "<"
  "<<"
  "<="
  "<>"
  "="
  ":="
  "=="
  ">"
  ">="
  ">>"
  "|"
  "~"
  "and"
  "in"
  "is"
  "not"
  "or"
] @operator

[
  "as"
  "assert"
  "async"
  "await"
  "break"
  "class"
  "continue"
  "def"
  "del"
  "elif"
  "else"
  "except"
  "exec"
  "finally"
  "for"
  "from"
  "global"
  "if"
  "import"
  "lambda"
  "nonlocal"
  "pass"
  "print"
  "raise"
  "return"
  "try"
  "while"
  "with"
  "yield"
] @keyword

; Mojo-specific declaration keywords. The grammar accepts each as an
; anonymous string token (see grammar.js: `fn` in function_definition,
; `raises` in raises_clause, etc.), so literal-token highlighting fires.

[
  "fn"
  "var"
  "let"
  "struct"
  "trait"
  "alias"
  "comptime"
] @keyword

; `raises` is wrapped in a single-token rule (raises_clause) by the
; grammar, so highlight it via the rule rather than the bare literal.

(raises_clause) @keyword

; Mojo argument-convention keywords. Appear only inside `mojo_parameter`
; (see grammar.js: argument_convention). Captured as @keyword.modifier so
; themes can color them distinctly from control-flow keywords.

[
  "owned"
  "borrowed"
  "inout"
  "mut"
  "read"
  "ref"
  "out"
  "deinit"
] @keyword.modifier
