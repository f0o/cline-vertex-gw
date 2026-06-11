package pipeline

import (
	"strings"
	"testing"
)

func TestCompressCodeBlocks_Go(t *testing.T) {
	syntaxCompressorEnabled = true

	// A realistic, longer Go code block to overcome marker overhead
	inputCode := "```go\npackage main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"math/rand\"\n\t\"time\"\n)\n\nfunc ProcessData(x int) (string, error) {\n\tfmt.Printf(\"processing %d\", x)\n\tfor i := 0; i < 100; i++ {\n\t\tx += rand.Intn(10)\n\t\tfmt.Printf(\"iteration %d: current x is %d\\n\", i, x)\n\t}\n\tlogSyntax.Infof(\"Data processing has completed with value x=%d\", x)\n\tif x < 0 {\n\t\treturn \"\", fmt.Errorf(\"invalid value calculated in sequence: %d\", x)\n\t}\n\tfmt.Println(\"final validation check completed successfully without errors\")\n\treturn \"success_data_processing_result\", nil\n}\n\nfunc main() {\n\tProcessData(10)\n\tfmt.Println(\"Program finished execution\")\n}\n```"

	compressed, saved := CompressCodeBlocks(inputCode)
	if saved <= 0 {
		t.Fatalf("expected Go code block to be compressed, got saved = %d. Compressed: %s", saved, compressed)
	}

	if !strings.Contains(compressed, "ProcessData(x int)") {
		t.Errorf("expected ProcessData function signature to be preserved, got: %s", compressed)
	}

	if !strings.Contains(compressed, "body elided (retrieve_elided_content") {
		t.Errorf("expected body elided placeholder, got: %s", compressed)
	}

	if !strings.Contains(compressed, "import ( /* collapsed imports") {
		t.Errorf("expected imports to be collapsed, got: %s", compressed)
	}
}

func TestCompressCodeBlocks_Python(t *testing.T) {
	syntaxCompressorEnabled = true

	// A realistic, longer Python code block to overcome marker overhead
	inputCode := "```python\nimport sys\nimport os\nfrom datetime import datetime\nimport json\nimport math\n\nclass DataProcessor:\n    def __init__(self, value):\n        self.value = value\n        self.time = datetime.now()\n        print(f\"Initializing DataProcessor with value {value} at time {self.time}\")\n        self.status = \"initialized\"\n\n    def run_calculation(self):\n        result = self.value * 2\n        for i in range(100):\n            result += math.sin(i) * 0.5\n            print(f\"iteration {i}: incremental result is {result}\")\n        print(f\"Final calculation finished. Result is {result}\")\n        self.status = \"completed\"\n        return result\n```"

	compressed, saved := CompressCodeBlocks(inputCode)
	if saved <= 0 {
		t.Fatalf("expected Python code block to be compressed, got saved = %d. Compressed: %s", saved, compressed)
	}

	if !strings.Contains(compressed, "class DataProcessor:") {
		t.Errorf("expected class definition to be preserved, got: %s", compressed)
	}

	if !strings.Contains(compressed, "def run_calculation(self):") {
		t.Errorf("expected method definition to be preserved, got: %s", compressed)
	}

	if !strings.Contains(compressed, "pass # body elided (retrieve_elided_content") {
		t.Errorf("expected body elided placeholder 'pass', got: %s", compressed)
	}

	if !strings.Contains(compressed, "import ... # collapsed python imports") {
		t.Errorf("expected imports to be collapsed, got: %s", compressed)
	}
}
