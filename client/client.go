/*
	Package client implements the class of a network client which can interact with a mix network.
*/

package client

import (
	"fmt"
	"net"
	"os"

	"anonymous-messaging/clientCore"
	"anonymous-messaging/networker"
	"anonymous-messaging/pki"
	"anonymous-messaging/config"
	"github.com/jmoiron/sqlx"
	"crypto/elliptic"
	"anonymous-messaging/helpers"
)

const (
	desiredRateParameter = 5
	pathLength           = 2
)

type ClientIt interface {
	networker.NetworkClient
	networker.NetworkServer
	SendMessage(message string, recipient config.MixPubs)
	ProcessPacket(packet []byte)
	Start()
	ReadInMixnetPKI()
	ReadInClientsPKI()
	ConnectToPKI(dbName string) *sqlx.DB
}

type Client struct {
	Host string
	Port string

	clientCore.CryptoClient

	ActiveMixes  []config.MixPubs
	OtherClients []config.ClientPubs

	listener *net.TCPListener

	pkiDir string
	Provider config.MixPubs
	Config config.ClientPubs
}

func (c *Client) SendMessage(message string, recipient config.ClientPubs) {
	mixSeq := c.GetRandomMixSequence(c.ActiveMixes, pathLength)

	var path config.E2EPath
	path.IngressProvider = c.Provider
	path.Mixes = mixSeq
	path.EgressProvider = *recipient.Provider
	path.Recipient = recipient

	delays := c.GenerateDelaySequence(desiredRateParameter, path.Len())

	packet, err := c.EncodeMessage(message, path, delays)
	if err != nil{
		panic(err)
	}

	err = c.Send(packet, path.IngressProvider.Host, path.IngressProvider.Port)
	if err != nil{
		fmt.Println("> Client sending FAILURE!")
		panic(err)
	}
}

func (c *Client) Send(packet []byte, host string, port string) error {

	conn, err := net.Dial("tcp", host+":"+port)

	if err != nil {
		fmt.Print("Error in Client connect: ", err.Error())
		os.Exit(1)
	} else {
		defer conn.Close()
	}

	_, err = conn.Write(packet)
	return err
}

func (c *Client) ListenForIncomingConnections() {
	for {
		conn, err := c.listener.Accept()

		if err != nil {
			fmt.Println("Error in connection accepting:", err.Error())
			os.Exit(1)
		}
		go c.HandleConnection(conn)
	}
}

func (c *Client) HandleConnection(conn net.Conn) {
	fmt.Println("> Handle Connection")

	buff := make([]byte, 1024)

	reqLen, err := conn.Read(buff)
	if err != nil {
		panic(err)
	}

	c.ProcessPacket(buff[:reqLen])
	conn.Close()
}

func (c *Client) ProcessPacket(packet []byte) []byte {
	fmt.Println("Processing packet: ", packet)
	return packet
}

func (c *Client) Start() {

	defer c.Run()

	c.ReadInClientsPKI(c.pkiDir)
	c.ReadInMixnetPKI(c.pkiDir)

}

func (c *Client) contactProvider() {
	fmt.Println("Sending to provider")
}

func (c *Client) Run() {
	fmt.Println("> Client is running")

	defer c.listener.Close()
	finish := make(chan bool)

	go func() {
		fmt.Println("Listening on " + c.Host + ":" + c.Port)
		c.ListenForIncomingConnections()
	}()


	go func() {
		c.SendMessage("Hello world, this is me", c.OtherClients[0])
	}()

	<-finish
}

func (c *Client) ReadInMixnetPKI(pkiName string) {
	fmt.Println("Reading network from pki:  ", pkiName)

	db, err := c.ConnectToPKI(pkiName)

	if err != nil{
		panic(err)
	}

	records, err := pki.QueryDatabase(db, "Mixes")

	if err != nil{
		panic(err)
	}

	for records.Next() {
		result := make(map[string]interface{})
		err := records.MapScan(result)

		if err != nil {
			panic(err)

		}
		pubs, err := config.MixPubsFromBytes(result["Config"].([]byte))
		if err != nil {
			panic(err)
		}

		c.ActiveMixes = append(c.ActiveMixes, pubs)
	}
	fmt.Println("> The mix network data is uploaded.")
}

func (c *Client) ReadInClientsPKI(pkiName string) {
	fmt.Println("Reading public information about clients")


	db, err := c.ConnectToPKI(pkiName)

	if err != nil{
		panic(err)
	}

	records, err := pki.QueryDatabase(db, "Clients")

	if err != nil {
		panic(err)
	}

	for records.Next() {
		result := make(map[string]interface{})
		err := records.MapScan(result)

		if err != nil {
			panic(err)
		}

		pubs, err := config.ClientPubsFromBytes(result["Config"].([]byte))
		if err != nil {
			panic(err)
		}
		c.OtherClients = append(c.OtherClients, pubs)
	}
	fmt.Println("> The clients data is uploaded.")
}

func (c *Client) ConnectToPKI(dbName string) (*sqlx.DB, error) {
	return pki.OpenDatabase(dbName, "sqlite3")
}

func SaveInPKI(c Client, pkiDir string) {
	fmt.Println("> Saving Client Public Info into Database")

	db, err := pki.OpenDatabase(pkiDir, "sqlite3")

	if err != nil {
		panic(err)
	}

	// TO DO: THIS SHOULD BE REMOVED AND DONE IS A PRE SETTING FILE

	params := make(map[string]string)
	params["Id"] = "TEXT"
	params["Typ"] = "TEXT"
	params["Config"] = "BLOB"
	pki.CreateTable(db, "Clients", params)


	configBytes, err := config.ClientPubsToBytes(c.Config)
	if err != nil {
		panic(err)
	}

	pki.InsertIntoTable(db, "Clients", c.Id, "Client", configBytes)
	fmt.Println("> Public info of the client saved in database")
	db.Close()
}

func NewClient(id, host, port string, pubKey []byte, prvKey []byte, pkiDir string, provider config.MixPubs) *Client {
	core := clientCore.CryptoClient{Id: id, PubKey: pubKey, PrvKey: prvKey, Curve: elliptic.P224()}

	c := Client{Host: host, Port: port, CryptoClient: core, Provider: provider, pkiDir: pkiDir}
	c.Config = config.ClientPubs{Id : c.Id, Host: c.Host, Port: c.Port, PubKey: c.PubKey, Provider: &c.Provider}

	SaveInPKI(c, pkiDir)


	addr, err := helpers.ResolveTCPAddress(c.Host, c.Port)
	if err != nil {
		panic(err)
	}
	c.listener, err = net.ListenTCP("tcp", addr)

	if err != nil {
		panic(err)
	}
	return &c
}
