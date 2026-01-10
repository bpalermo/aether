package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func newStringValue() *wrapperspb.StringValue {
	return &wrapperspb.StringValue{}
}

func TestNewFileStorage(t *testing.T) {
	storage := NewFileStorage[*wrapperspb.StringValue]("/tmp/test", newStringValue)

	assert.NotNil(t, storage)
	assert.Equal(t, "/tmp/test", storage.basePath)
	assert.NotNil(t, storage.cache)
	assert.Empty(t, storage.cache)
}

func TestCachedFileStorage_AddResource(t *testing.T) {
	dir := t.TempDir()
	storage := NewFileStorage[*wrapperspb.StringValue](dir, newStringValue)

	tests := []struct {
		name    string
		key     string
		value   string
		wantErr bool
	}{
		{
			name:    "add new resource",
			key:     "test1",
			value:   "value1",
			wantErr: false,
		},
		{
			name:    "add another resource",
			key:     "test2",
			value:   "value2",
			wantErr: false,
		},
		{
			name:    "overwrite existing resource",
			key:     "test1",
			value:   "updated_value1",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := &wrapperspb.StringValue{Value: tt.value}
			err := storage.AddResource(tt.key, msg)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)

				// Verify in cache
				cached, ok := storage.cache[tt.key]
				assert.True(t, ok)
				assert.Equal(t, tt.value, cached.Value)

				// Verify on disk
				filePath := filepath.Join(dir, tt.key+".pb")
				assert.FileExists(t, filePath)
			}
		})
	}
}

func TestCachedFileStorage_GetResource(t *testing.T) {
	dir := t.TempDir()
	storage := NewFileStorage[*wrapperspb.StringValue](dir, newStringValue)

	// Add resources
	msg1 := &wrapperspb.StringValue{Value: "value1"}
	msg2 := &wrapperspb.StringValue{Value: "value2"}
	require.NoError(t, storage.AddResource("key1", msg1))
	require.NoError(t, storage.AddResource("key2", msg2))

	tests := []struct {
		name      string
		key       string
		wantValue string
		wantErr   bool
	}{
		{
			name:      "get existing resource from cache",
			key:       "key1",
			wantValue: "value1",
			wantErr:   false,
		},
		{
			name:      "get another existing resource",
			key:       "key2",
			wantValue: "value2",
			wantErr:   false,
		},
		{
			name:    "get non-existent resource",
			key:     "nonexistent",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := storage.GetResource(tt.key)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantValue, result.Value)
			}
		})
	}
}

func TestCachedFileStorage_GetResourceFromDisk(t *testing.T) {
	dir := t.TempDir()

	// Create a file on disk without going through storage
	msg := &wrapperspb.StringValue{Value: "disk_value"}
	data, err := proto.Marshal(msg)
	require.NoError(t, err)

	filePath := filepath.Join(dir, "disk_key.pb")
	err = os.WriteFile(filePath, data, 0644)
	require.NoError(t, err)

	// Create storage after the file exists
	storage := NewFileStorage[*wrapperspb.StringValue](dir, newStringValue)

	// Get resource (should load from disk)
	result, err := storage.GetResource("disk_key")
	assert.NoError(t, err)
	assert.Equal(t, "disk_value", result.Value)

	// Verify it's now in the cache
	cached, ok := storage.cache["disk_key"]
	assert.True(t, ok)
	assert.Equal(t, "disk_value", cached.Value)
}

func TestCachedFileStorage_RemoveResource(t *testing.T) {
	dir := t.TempDir()
	storage := NewFileStorage[*wrapperspb.StringValue](dir, newStringValue)

	// Add resources
	msg := &wrapperspb.StringValue{Value: "value_to_remove"}
	require.NoError(t, storage.AddResource("remove_key", msg))

	// Verify it exists
	filePath := filepath.Join(dir, "remove_key.pb")
	assert.FileExists(t, filePath)
	_, ok := storage.cache["remove_key"]
	assert.True(t, ok)

	// Remove resource
	err := storage.RemoveResource("remove_key")
	assert.NoError(t, err)

	// Verify it's removed from disk and cache
	assert.NoFileExists(t, filePath)
	_, ok = storage.cache["remove_key"]
	assert.False(t, ok)

	// Try to remove non-existent resource
	err = storage.RemoveResource("nonexistent")
	assert.NoError(t, err)
}

func TestCachedFileStorage_LoadAll(t *testing.T) {
	dir := t.TempDir()

	// Create multiple files on disk
	files := map[string]string{
		"file1": "value1",
		"file2": "value2",
		"file3": "value3",
	}

	for key, value := range files {
		msg := &wrapperspb.StringValue{Value: value}
		data, err := proto.Marshal(msg)
		require.NoError(t, err)

		filePath := filepath.Join(dir, key+".pb")
		err = os.WriteFile(filePath, data, 0644)
		require.NoError(t, err)
	}

	// Create a non-pb file that should be ignored
	err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("ignore"), 0644)
	require.NoError(t, err)

	// Create storage and load all
	storage := NewFileStorage[*wrapperspb.StringValue](dir, newStringValue)
	_, err = storage.LoadAll()
	assert.NoError(t, err)

	// Verify all files are loaded into cache
	assert.Len(t, storage.cache, 3)
	for key, expectedValue := range files {
		cached, ok := storage.cache[key]
		assert.True(t, ok, "key %s should be in cache", key)
		assert.Equal(t, expectedValue, cached.Value)
	}
}

func TestCachedFileStorage_LoadAllEmptyDir(t *testing.T) {
	dir := t.TempDir()
	storage := NewFileStorage[*wrapperspb.StringValue](dir, newStringValue)

	_, err := storage.LoadAll()
	assert.NoError(t, err)
	assert.Empty(t, storage.cache)
}

func TestCachedFileStorage_LoadAllNonExistentDir(t *testing.T) {
	storage := NewFileStorage[*wrapperspb.StringValue]("/nonexistent/dir", newStringValue)

	_, err := storage.LoadAll()
	assert.Error(t, err)
	assert.Empty(t, storage.cache)
}

func TestCachedFileStorage_ConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	storage := NewFileStorage[*wrapperspb.StringValue](dir, newStringValue)

	done := make(chan bool)

	// Concurrent writes
	for i := 0; i < 10; i++ {
		go func(n int) {
			key := fmt.Sprintf("concurrent_%d", n)
			value := fmt.Sprintf("value_%d", n)
			msg := &wrapperspb.StringValue{Value: value}
			err := storage.AddResource(key, msg)
			assert.NoError(t, err)
			done <- true
		}(i)
	}

	// Concurrent reads
	for i := 0; i < 10; i++ {
		go func(n int) {
			key := fmt.Sprintf("concurrent_%d", n)
			// May or may not find the resource depending on timing
			_, _ = storage.GetResource(key)
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < 20; i++ {
		<-done
	}

	// Verify all resources are present
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("concurrent_%d", i)
		value := fmt.Sprintf("value_%d", i)
		result, err := storage.GetResource(key)
		assert.NoError(t, err)
		assert.Equal(t, value, result.Value)
	}
}
