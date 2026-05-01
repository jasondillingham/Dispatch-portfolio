package main
import (
  "fmt"
  "os"
  "dispatch/internal/pdftext"
)
func main() {
  f, _ := os.Open(os.Args[1])
  defer f.Close()
  t, _ := pdftext.Extract(f)
  fmt.Println(t)
}
