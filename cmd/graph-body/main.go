package main
import (
	"encoding/json"
	"fmt"
	"os"
	"dispatch/internal/graph"
)
func main() {
	gc, _ := graph.NewClient()
	m, err := gc.GetMessageDetail(os.Args[1], os.Args[2])
	if err != nil { fmt.Println("err:", err); return }
	b, _ := json.MarshalIndent(m.Body, "", "  ")
	fmt.Println(string(b))
}
