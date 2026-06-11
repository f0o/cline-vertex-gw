# 1a. Normalize Whitespace

The **Normalize Whitespace** stage is positioned at step `1a` of the optimization pipeline. It executes standard, completely lossless layout pruning to shrink and clean up formatting overhead across all messages.

---

## 🔍 Why It Matters

Source code files, terminal command outputs, and user prompts frequently carry excessive blank lines, horizontal indentations, carriage returns (`\r\n`), and Byte Order Marks (BOMs). 

While these formatting elements are useful for local human readability in editors, they represent **pure token bloat** to large language models. A series of 5 blank lines represents 5 separate newline tokens that add zero semantic value.

**Normalize Whitespace** executes aggressive, code-safe formatting cleanups. Running this stage *first* in the text processing sequence ensures that downstream byte-budget limits and deduplication hashers calculate sizes on normalized payloads, maximizing prompt density.

---

## ⚙️ How It Works

This stage is implemented in `pkg/pipeline/normalize.go` and processes the text parts of all messages (including user turns, model turns, and the system prompt) using Go's high-performance string libraries:

### 1. Byte Order Mark (BOM) Stripping
It detects and strips UTF-8 BOM bytes (`\xef\xbb\xbf`) if present at the start of any text part, preventing tokenization desync.

### 2. Line Ending Unification
It converts all carriage return line endings (`\r\n` or `\r`) to standard Unix line feeds (`\n`), unifying format standards.

### 3. Trailing Horizontal Space Trimming
It walks every line in a text part and trims trailing horizontal spaces or tabs, without touching leading indentation. This ensures **indented source code block structures remain 100% untouched and functional**.

### 4. Blank Line Capping
It detects runs of consecutive empty lines and caps them at a maximum of **2 blank lines**. Any trailing empty lines at the very end of a text part are removed entirely.

---

## 🎛️ Configuration Parameters

The following environment variables govern this stage:
*   `GW_NORMALIZE_WHITESPACE` — Master switch. Set to `false` or `0` to disable layout normalization. Defaults to `true` on Balanced.

---

## 📝 Example

### Before Optimization
```text
Hello, world!  \r
\r
\r
\r
\r
Let's look at this indented Go code:
    func main() {    
        println("Hello")    
    }    
\r
\r
```

### After Optimization
```text
Hello, world!

Let's look at this indented Go code:
    func main() {
        println("Hello")
    }
```
*   **Result**: Carriage returns are stripped, trailing horizontal whitespace on lines is removed, the 4 blank lines are collapsed, and the trailing newlines are removed, all while preserving the leading indents of the Go code.
