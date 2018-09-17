package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var cataloguePat = regexp.MustCompile("/catalogue/([\\w]+)/([-\\w]+)\\.jpg")

type LocalStore struct {
	baseDir  string
	lastSync time.Time
}

func (store *LocalStore) guestDir() string {
	return filepath.Join(store.baseDir, "guests")
}

func (store *LocalStore) catalogueDir() string {
	return filepath.Join(store.baseDir, "catalogue")
}

func (store *LocalStore) Setup() error {
	_, err := os.Stat(store.guestDir())
	if os.IsNotExist(err) {
		if err := os.Mkdir(store.guestDir(), 0755); err != nil {
			return err
		}
	}
	return nil
}

func (store *LocalStore) SaveGuest(img []byte, idx int) error {
	path := filepath.Join(store.guestDir(), fmt.Sprintf("%s-%d.jpg", time.Now().Format("20060102-150405"), idx))
	return ioutil.WriteFile(path, img, 0644)
}

func (store *LocalStore) ReadImage(key FaceKey) ([]byte, error) {
	path := filepath.Join(store.catalogueDir(), key.Name, key.Index+".jpg")
	return ioutil.ReadFile(path)
}

func (store *LocalStore) Watch(synccb SyncFunc) error {
	if _, err := os.Stat(store.catalogueDir()); err != nil {
		if os.IsNotExist(err) {
			log.Print("Stop to watch catalogue folder")
			return nil
		}
		return err
	}
	log.Print("Start to watch catalogue folder updates.")

	go func() {
		if err := store.sync(synccb); err != nil {
			log.Print("LocalStore.Watch: ", err)
			return
		}
		time.Sleep(10 * time.Second)
	}()
	return nil
}

func (store *LocalStore) sync(synccb SyncFunc) error {
	keys := []FaceKey{}
	var lastMod time.Time
	err := filepath.Walk(store.catalogueDir(), func(path string, info os.FileInfo, err error) error {
		if lastMod.After(info.ModTime()) {
			lastMod = info.ModTime()
		}

		m := cataloguePat.FindStringSubmatch(path)
		if len(m) != 3 {
			return nil
		}

		keys = append(keys, FaceKey{m[1], m[2]})
		return nil
	})
	if err != nil {
		return err
	}
	if store.lastSync.After(lastMod) {
		// Do nothing if there is no updates in the catalogue folder.
		return nil
	}

	if err := synccb(keys); err != nil {
		return err
	}
	// Update timestamp only when synching stage succeeded.
	store.lastSync = lastMod
	return nil
}
