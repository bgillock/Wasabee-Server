package wasabi

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"

	"crypto/sha256"
)

// MaxFilesize is the maximum file size for simple uploads (1MB)
const MaxFilesize = 1024 * 1024 // 1MB

// SimpleDocument specifies the content and metadata of a simple-style (qbin style) draw.
type SimpleDocument struct {
	ID         string
	Content    string
	Upload     time.Time
	Expiration time.Time
	Views      int
}

// Store a document object in the database.
func (document *SimpleDocument) Store() error {
	// Generate a name that doesn't exist yet
	name, err := GenerateSafeName()
	if err != nil {
		Log.Errorf("GenerateSafeName: %s", err)
		return err
	}
	document.ID = name

	// Round the timestamps on the object. Won't affect the database, but we want consistency.
	document.Upload = time.Now().Round(time.Second)
	document.Expiration = document.Expiration.Round(time.Second)

	// Normalize new lines
	document.Content = strings.Trim(strings.Replace(strings.Replace(document.Content, "\r\n", "\n", -1), "\r", "\n", -1), "\n") + "\n"

	// Don't accept binary files
	if strings.Contains(document.Content, "\x00") {
		Log.Debug("file contails NULL bytes")
		return errors.New("file contains 0x00 bytes")
	}

	var expiration interface{}
	if (document.Expiration != time.Time{}) {
		expiration = document.Expiration.UTC().Format("2006-01-02 15:04:05")
	}

	// Server-Side Encryption
	key, err := scrypt.Key([]byte(document.ID), []byte(document.Upload.UTC().Format("2006-01-02 15:04:05")), 16384, 8, 1, 24)
	if err != nil {
		Log.Errorf("Invalid script parameters: %s", err)
	}
	data, err := encrypt([]byte(document.Content), key)
	if err != nil {
		Log.Errorf("AES error: %s", err)
		return err
	}

	databaseID := sha256.Sum256([]byte(document.ID))

	// Write the document to the database
	_, err = db.Exec(
		"INSERT INTO document (id, content, upload, expiration, views) VALUES (?, ?, ?, ?, 0)",
		hex.EncodeToString(databaseID[:]),
		string(data),
		document.Upload.UTC().Format("2006-01-02 15:04:05"), // don't use NOW() since this is used in the key...
		expiration)
	if err != nil {
		Log.Error(err)
		return err
	}
	return nil
}

// Request a document from the database by its ID.
func Request(id string) (SimpleDocument, error) {
	doc := SimpleDocument{ID: id}
	var views int
	var upload, expiration sql.NullString
	databaseID := sha256.Sum256([]byte(id))
	err := db.QueryRow("SELECT content, upload, expiration, views FROM document WHERE id = ?", hex.EncodeToString(databaseID[:])).
		Scan(&doc.Content, &upload, &expiration, &views)
	if err != nil {
		if err.Error() != NoRows {
			Log.Warningf("Error retrieving document: %s", err)
		}
		return SimpleDocument{}, err
	}

	_, err = db.Exec("UPDATE document SET views = views + 1 WHERE id = ?", hex.EncodeToString(databaseID[:]))
	if err != nil {
		Log.Error("unable to update document view count")
	}
	doc.Views = views

	doc.Upload, _ = time.Parse("2006-01-02 15:04:05", upload.String)

	key, err := scrypt.Key([]byte(id), []byte(doc.Upload.UTC().Format("2006-01-02 15:04:05")), 16384, 8, 1, 24)
	if err != nil {
		Log.Errorf("Invalid script parameters: %s", err)
		return SimpleDocument{}, err
	}
	data, err := decrypt([]byte(doc.Content), key)
	if err != nil && !(err.Error() == "cipher: message authentication failed" && !strings.Contains(doc.Content, "\000")) {
		Log.Errorf("AES error: %s", err)
		return SimpleDocument{}, err
	} else if err == nil {
		doc.Content = string(data)
	}

	if expiration.Valid {
		doc.Expiration, err = time.Parse("2006-01-02 15:04:05", expiration.String)
		if doc.Expiration.Before(time.Unix(0, 1)) {
			if doc.Views > 0 {
				// Volatile document
				_, err = db.Exec("DELETE FROM document WHERE id = ?", hex.EncodeToString(databaseID[:]))
				if err != nil {
					Log.Errorf("couldn't delete volatile document: %s", err)
				}
			}
		} else {
			if err != nil {
				return SimpleDocument{}, err
			}
			if doc.Expiration.Before(time.Now()) {
				return SimpleDocument{}, errors.New("the document has expired")
			}
		}
	}

	return doc, nil
}

func simpleDocClean() {
	// do it this way to get RowsAffected
	stmt, _ := db.Prepare("DELETE FROM document WHERE expiration < CURRENT_TIMESTAMP AND expiration > FROM_UNIXTIME(0)")

	result, err := stmt.Exec()
	if err != nil {
		Log.Errorf("couldn't execute cleanup statement: %s", err)
	} else {
		n, err := result.RowsAffected()
		if err == nil && n > 0 {
			Log.Debugf("cleaned up %d documents", n)
		}
	}
}
