package storage

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/fsnotify/fsnotify"
)

type InFile struct {
	ims      Storage
	filename string
	seek     int64
}

type shortedURL struct {
	Sid     ShortID `json:"id"`
	Furl    FullURL `json:"url"`
	User    User    `json:"user"`
	Deleted bool    `json:"deleted"`
}

func NewInFile(filename string) (Storage, error) {
	ifs := InFile{ims: NewInMemory(), filename: filename, seek: 0}

	if err := ifs.readUpdates(); err != nil {
		return nil, err
	}

	go ifs.backgroundUpdate()

	return &ifs, nil
}

func (ifs *InFile) Get(ctx context.Context, sid ShortID) (FullURL, error) {
	return ifs.ims.Get(ctx, sid)
}

func (ifs *InFile) Save(ctx context.Context, sid ShortID, furl FullURL) error {
	if err := ifs.ims.Save(ctx, sid, furl); err != nil {
		return err
	}

	user, _ := GetUser(ctx)
	update := []shortedURL{}
	update = append(update, shortedURL{Sid: sid, Furl: furl, User: user})
	go ifs.writeUpdates(update)

	return nil
}

func (ifs *InFile) Put(ctx context.Context, furl FullURL) (ShortID, error) {
	sid := Short(furl)

	return sid, ifs.Save(ctx, sid, furl)
}

func (ifs *InFile) PutBatch(ctx context.Context, batch BatchRequest) (BatchResponse, error) {
	response, err := ifs.ims.PutBatch(ctx, batch)
	if err != nil {
		return response, err
	}

	user, _ := GetUser(ctx)
	update := []shortedURL{}
	for corrid, sid := range response {
		update = append(update, shortedURL{Sid: sid, Furl: batch[corrid], User: user})
	}
	go ifs.writeUpdates(update)

	return response, nil
}

func (ifs *InFile) GetURLs(ctx context.Context) (SavedURLs, error) {
	return ifs.ims.GetURLs(ctx)
}

func (ifs *InFile) DeleteURLs(ctx context.Context, sids []ShortID) error {
	if _, err := GetUser(ctx); err != nil {
		return err
	}

	go func() {
		_ = ifs.AsyncDeleteURLs(ctx, sids)
	}()

	return nil
}

func (ifs *InFile) AsyncDeleteURLs(ctx context.Context, sids []ShortID) []ShortID {
	result := ifs.ims.AsyncDeleteURLs(ctx, sids)

	user, err := GetUser(ctx)
	if err != nil {
		return result
	}

	update := []shortedURL{}
	for _, sid := range result {
		update = append(update, shortedURL{Sid: sid, Furl: DefaultFullURL, User: user, Deleted: true})
	}
	ifs.writeUpdates(update)
	return result
}

func (ifs *InFile) NewUser(ctx context.Context) (User, error) {
	return ifs.ims.NewUser(ctx)
}

func (ifs *InFile) AddUser(ctx context.Context, user User) {
	ifs.ims.AddUser(ctx, user)
}

func (ifs *InFile) Ping(ctx context.Context) bool {
	if !ifs.ims.Ping(ctx) {
		return false
	}

	file, err := os.OpenFile(ifs.filename, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return false
	}
	file.Close()

	return true
}

func (ifs *InFile) backgroundUpdate() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Println("storage: infile: Async: cannot initialize async worker, async disabled: ", err.Error())
		return
	}
	defer watcher.Close()

	if err = watcher.Add(ifs.filename); err != nil {
		log.Println("storage: infile: Async: cannot initialize notifies for storage file, async disabled: ", err.Error())
		return
	}
	for {
		select {
		case event := <-watcher.Events:
			if event.Op&fsnotify.Write != fsnotify.Write {
				continue
			}
			if err := ifs.readUpdates(); err != nil {
				log.Println("storage: infile: ReadUpdates error:", err.Error())
			}
		case err := <-watcher.Errors:
			log.Println("storage: infile: Async error:", err.Error())
		}
	}
}

func (ifs *InFile) writeUpdates(s []shortedURL) {
	file, err := os.OpenFile(ifs.filename, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		log.Println("storage: infile: writeUpdate: cannot open storage file:", err.Error())
		return
	}
	defer file.Close()

	writer := bufio.NewWriter(file)

	data := []byte{}
	for _, v := range s {
		marshaled, _ := json.Marshal(v)
		data = append(data, marshaled...)
		data = append(data, '\n')
	}

	n, err := writer.Write(data)
	if err != nil {
		log.Println("storage: infile: writeUpdate: cannot write to IO buffer:", err.Error())
		return
	}
	if err := writer.Flush(); err != nil {
		log.Println("storage: infile: writeUpdate: cannot write to file:", err.Error())
		return
	}

	ifs.seek += int64(n)
}

func (ifs *InFile) readUpdates() error {
	file, err := os.OpenFile(ifs.filename, os.O_RDONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return err
	}

	if fileInfo.Size() < ifs.seek {
		// reread from start
		ifs.seek = 0
	}

	if _, err = file.Seek(ifs.seek, io.SeekStart); err != nil {
		return err
	}

	buffered := bufio.NewReader(file)
	var s shortedURL
	var line []byte
	for {
		if line, err = buffered.ReadBytes('\n'); err != nil {
			break
		}
		if err := json.Unmarshal(line, &s); err != nil {
			log.Println("storage: infile: readUpdates: cannot Unmarshal shortedURL:", err.Error())
			continue
		}

		ctx := context.WithValue(context.Background(), UserCtxKey{}, fmt.Sprint(s.User))
		if s.Deleted {
			ifs.ims.AsyncDeleteURLs(ctx, []ShortID{s.Sid})
			continue
		}

		if err := ifs.ims.Save(ctx, s.Sid, s.Furl); err != nil {
			log.Println("storage: infile: readUpdates: cannot Save in memory:", err.Error())
			continue
		}
	}

	if fileInfo, err = file.Stat(); err != nil {
		return err
	}

	ifs.seek = fileInfo.Size()
	return nil
}
