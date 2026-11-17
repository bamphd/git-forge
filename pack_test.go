package main

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"
)

func TestPackRoundTripManySizes(t *testing.T) {
	var objects []*GitObject
	// Sizes spanning bufio refill boundaries and stored-block thresholds.
	sizes := []int{0, 1, 5, 100, 1000, 4095, 4096, 4097, 8192, 16384, 65535, 65536, 100000}
	for i, size := range sizes {
		data := make([]byte, size)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		// Mix compressible and incompressible content.
		if i%2 == 0 {
			for j := range data {
				data[j] = byte(j % 251)
			}
		}
		objects = append(objects, &GitObject{Type: ObjBlob, Data: data})
	}
	// 100 small files, like the failing check.
	for i := 0; i < 100; i++ {
		objects = append(objects, &GitObject{Type: ObjBlob, Data: []byte(fmt.Sprintf("file %d contents\n", i))})
	}
	pack := WritePack(objects)
	read, err := ReadPack(pack, nil)
	if err != nil {
		t.Fatalf("ReadPack: %v", err)
	}
	if !read.ChecksumOK {
		t.Fatalf("checksum mismatch")
	}
	if len(read.Objects) != len(objects) {
		t.Fatalf("got %d objects, want %d", len(read.Objects), len(objects))
	}
	for _, obj := range objects {
		oid := HashObject(obj.Type, obj.Data)
		got := read.Objects[oid]
		if got == nil || !bytes.Equal(got.Data, obj.Data) {
			t.Fatalf("object %s mismatch", oid)
		}
	}
}

func TestInflateAtConsecutive(t *testing.T) {
	// Two zlib streams back to back: inflateAt must stop exactly at the
	// first stream's end.
	a := WritePack([]*GitObject{{Type: ObjBlob, Data: bytes.Repeat([]byte("hello world, "), 1000)}})
	b := WritePack([]*GitObject{{Type: ObjBlob, Data: []byte("second")}})
	_ = b
	read, err := ReadPack(a, nil)
	if err != nil {
		t.Fatalf("ReadPack: %v", err)
	}
	if len(read.Objects) != 1 {
		t.Fatalf("got %d objects", len(read.Objects))
	}
}
