package kbaseauthcontroller

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/phayes/freeport"
)

const (
	serverClass = "us.kbase.test.auth2.StandaloneAuthServer"
)

// Params are Parameters for creating a KBase Auth2 service (https://github.com/kbase/auth2)
// controller.
type Params struct {
	// JarsDir is the path to the /lib/jars directory of the
	JarsDir string
	// MongoHost is the mongo host.
	MongoHost string
	// MongoDatabase is the database to use for auth data.
	MongoDatabase string
	// RootTempDir is where temporary files should be placed.
	RootTempDir string
}

// Controller is a KBase auth service controller.
type Controller struct {
	port    int
	tempDir string
	cmd     *exec.Cmd
}

// New creates a new controller.
func New(p Params) (*Controller, error) {
	classPath, err := getClassPath(p.JarsDir)
	if err != nil {
		return nil, err
	}
	tdir := filepath.Join(p.RootTempDir, "AuthController-"+uuid.New().String())
	templateDir := filepath.Join(tdir, "templates")
	err = os.MkdirAll(templateDir, 0700)
	if err != nil {
		return nil, err
	}
	err = installTemplates(p.JarsDir, templateDir)
	if err != nil {
		return nil, err
	}
	outfile, err := os.Create(filepath.Join(tdir, "output.txt"))
	if err != nil {
		return nil, err
	}
	port, err := freeport.GetFreePort()
	if err != nil {
		return nil, err
	}
	strport := strconv.Itoa(port)
	cmdargs := []string{
		"-classpath", classPath,
		"-DAUTH2_TEST_MONGOHOST=" + p.MongoHost,
		"-DAUTH2_TEST_MONGODB=" + p.MongoDatabase,
		"-DAUTH2_TEST_TEMPLATE_DIR=" + templateDir,
		serverClass,
		strport,
	}
	cmd := exec.Command("java", cmdargs...)
	cmd.Stdout = outfile
	cmd.Stderr = outfile
	err = cmd.Start()
	if err != nil {
		return nil, err
	}
	err = waitForStartup(strport)
	if err != nil {
		return nil, err
	}

	return &Controller{port, tdir, cmd}, nil
}

func waitForStartup(port string) error {
	var startupErr error
	for i := 0; i < 40; i++ {
		startupErr = nil
		time.Sleep(1 * time.Second) // wait for server to start
		req, startupErr := http.NewRequest(http.MethodGet, "http://localhost:"+port, nil)
		if startupErr == nil {
			res, startupErr := http.DefaultClient.Do(req)
			if startupErr == nil {
				if res.StatusCode == 200 {
					// could check body to make sure it's the auth server, but seems unnecessary
					res.Body.Close()
					break
				} else {
					buf := new(bytes.Buffer)
					buf.ReadFrom(res.Body)
					res.Body.Close()
					startupErr = errors.New(buf.String())
				}
			}
		}
	}
	return startupErr
}

func getClassPath(jarsDir string) (string, error) {
	jarsDir, err := filepath.Abs(jarsDir)
	if err != nil {
		return "", err
	}
	cp := []string(nil)
	for _, j := range jars { // global variable, yech
		jpath := path.Join(jarsDir, j)
		if _, err := os.Stat(jpath); os.IsNotExist(err) {
			return "", fmt.Errorf("Jar %v does not exist", jpath)
		}
		cp = append(cp, jpath)
	}
	return strings.Join(cp, ":"), nil
}

func installTemplates(jarsDir string, templateDir string) error {
	templateZip := path.Join(jarsDir, authTemplates)
	arch, err := zip.OpenReader(templateZip) // global variable, yech
	if err != nil {
		return err
	}
	for _, f := range arch.File {
		name := f.FileHeader.Name
		if !strings.HasSuffix(name, "/") { // not a directory
			name = path.Clean(name)
			if path.IsAbs(name) || strings.HasPrefix(name, "..") {
				return fmt.Errorf("Zip file %v contains files outside the zip directory - "+
					"this is a sign of a malicious zip file", templateZip)
			}
			target, err := filepath.Abs(path.Join(templateDir, name))
			if err != nil {
				return err
			}
			os.MkdirAll(path.Dir(target), 0600)
			r, err := f.Open()
			if err != nil {
				return err
			}
			f, err := os.Create(target)

			io.Copy(f, r)
			r.Close()
			f.Close()
		}
	}
	return nil
}

// GetPort returns the port on which MongoDB is listening.
func (c *Controller) GetPort() int {
	return c.port
}

// Destroy destroys the controller. If deleteTempDir is true, all files created by the controller
// will be removed.
func (c *Controller) Destroy(deleteTempDir bool) error {
	err := c.cmd.Process.Kill()
	if err != nil {
		return err
	}
	c.cmd.Wait()
	if err != nil {
		return err
	}
	if deleteTempDir {
		os.RemoveAll(c.tempDir)
	}
	return nil
}

// CreateTestUser creates a test user in the auth system
func (c *Controller) CreateTestUser(username string, displayname string) error {
	ep, _ := url.Parse("api/V2/testmodeonly/user")
	aurl := c.testURL().ResolveReference(ep)
	body := map[string]interface{}{"user": username, "display": displayname}
	_, err := sendJSON(http.MethodPost, aurl, body)
	return err
}

// CreateTestToken creates a token for the given user
func (c *Controller) CreateTestToken(username string) (string, error) {
	ep, _ := url.Parse("api/V2/testmodeonly/token")
	aurl := c.testURL().ResolveReference(ep)
	body := map[string]interface{}{"user": username, "type": "Login"}
	retbody, err := sendJSON(http.MethodPost, aurl, body)
	if err != nil {
		return "", err
	}
	var bd map[string]interface{}
	err = json.Unmarshal(*retbody, &bd)
	if err != nil {
		return "", err
	}
	return bd["token"].(string), nil
}

// CreateTestRole creates a role in the auth system
func (c *Controller) CreateTestRole(role string, description string) error {
	ep, _ := url.Parse("api/V2/testmodeonly/customroles")
	aurl := c.testURL().ResolveReference(ep)
	body := map[string]interface{}{"id": role, "description": description}
	_, err := sendJSON(http.MethodPost, aurl, body)
	return err
}

// SetTestUserRoles sets custom roles for a test user and removes all built-in roles.
func (c *Controller) SetTestUserRoles(username string, roles *[]string) error {
	ep, _ := url.Parse("api/V2/testmodeonly/userroles")
	aurl := c.testURL().ResolveReference(ep)
	body := map[string]interface{}{"user": username, "customroles": roles}
	_, err := sendJSON(http.MethodPut, aurl, body)
	return err
}

func sendJSON(method string, u *url.URL, body map[string]interface{}) (*[]byte, error) {
	js, _ := json.Marshal(body)
	req, err := http.NewRequest(method, u.String(), bytes.NewBuffer(js))
	if err != nil {
		return nil, err
	}
	req.Header.Add("content-type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	err = checkError(res)
	if err != nil {
		return nil, err
	}
	buf, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return &buf, nil
}

func checkError(resp *http.Response) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Printf("Response code: %v\n", resp.StatusCode)
		buf, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		s := string(buf)
		fmt.Println(s)
		if len(s) > 200 { // could be unicode, but meh
			s = s[:200]
		}
		return errors.New(s)
	}
	return nil
}

func (c *Controller) testURL() *url.URL {
	authURL, _ := url.Parse("http://localhost:" + strconv.Itoa(c.port) + "/testmode/")
	return authURL
}
