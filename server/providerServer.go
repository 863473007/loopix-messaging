package server

import (
	"anonymous-messaging/node"
	"net"
	"anonymous-messaging/networker"
	"os"
	"fmt"
	"bytes"
	"anonymous-messaging/config"
	"io/ioutil"
	"anonymous-messaging/helpers"
	log "github.com/sirupsen/logrus"
	"errors"
	"anonymous-messaging/sphinx"
	"github.com/protobuf/proto"
)

const (
	ASSIGNE_FLAG = "\xA2"
	COMM_FLAG = "\xC6"
	TOKEN_FLAG = "xA9"
	PULL_FLAG = "\xFF"
)

type ProviderIt interface {
	networker.NetworkServer
	networker.NetworkClient
}

type ProviderServer struct {
	Host string
	Port string
	node.Mix
	listener *net.TCPListener

	assignedClients map[string]ClientRecord

	Config config.MixConfig
}

type ClientRecord struct {
	Id string
	Host string
	Port string
	PubKey []byte
	Token []byte
}

/*
	Start function creates the loggers for capturing the info and error logs
	and starts the listening server. Function returns an error
	signaling whether any operation was unsuccessful
 */
func (p *ProviderServer) Start() error{

	p.Run()

	return nil
}

/*
	Function opens the listener to start listening on provider's host and port
 */
func (p *ProviderServer) Run() {

	defer p.listener.Close()
	finish := make(chan bool)

	go func() {
		log.WithFields(log.Fields{"id" : p.Id}).Info(fmt.Sprintf("Listening on %s", p.Host + ":" + p.Port))
		p.ListenForIncomingConnections()
	}()

	<-finish
}

/*
	Function processes the received sphinx packet, performs the
	unwrapping operation and checks whether the packet should be
	forwarded or stored. If the processing was unsuccessful and error is returned.
 */
func (p *ProviderServer) ReceivedPacket(packet []byte) error{
	log.WithFields(log.Fields{"id" : p.Id}).Info("Received new sphinx packet")

	c := make(chan []byte)
	cAdr := make(chan sphinx.Hop)
	cFlag := make(chan string)
	errCh := make(chan error)

	go p.ProcessPacket(packet, c, cAdr, cFlag, errCh)
	dePacket := <-c
	nextHop := <- cAdr
	flag := <- cFlag
	err := <- errCh

	if err != nil{
		return err
	}

	switch flag {
	case "\xF1":
		err = p.ForwardPacket(dePacket, nextHop.Address)
		if err != nil{
			return err
		}
	case "\xF0":
		err = p.StoreMessage(dePacket, nextHop.Id, "TMP_MESSAGE_ID")
		if err != nil{
			return err
		}
	default:
		log.WithFields(log.Fields{"id" : p.Id}).Info("Sphinx packet flag not recognised")
	}

	return nil
}

func (p *ProviderServer) ForwardPacket(sphinxPacket []byte, address string) error{
	packetBytes, err := config.WrapWithFlag(COMM_FLAG, sphinxPacket)
	if err != nil{
		return err
	}

	err = p.Send(packetBytes, address)
	if err != nil{
		return err
	}
	log.WithFields(log.Fields{"id" : p.Id}).Info(" Forwarded sphinx packet")
	return nil
}

/*
	Function opens a connection with selected network address
	and send the passed packet. If connection failed or
	the packet could not be send, an error is returned
*/
func (p *ProviderServer) Send(packet []byte, address string) error {

	conn, err := net.Dial("tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()

	conn.Write(packet)
	return nil
}


/*
	Function responsible for running the listening process of the server;
	The providers listener accepts incoming connections and
	passes the incoming packets to the packet handler.
	If the connection could not be accepted an error
	is logged into the log files, but the function is not stopped
*/
func (p *ProviderServer) ListenForIncomingConnections() {
	for {
		conn, err := p.listener.Accept()

		if err != nil {
			log.WithFields(log.Fields{"id" : p.Id}).Error(err)
		} else {
			log.WithFields(log.Fields{"id" : p.Id}).Info(fmt.Sprintf(" Received new connection from %s", conn.RemoteAddr()))
			errs := make(chan error, 1)
			go p.HandleConnection(conn, errs)
			err = <- errs
			if err != nil{
				log.WithFields(log.Fields{"id" : p.Id}).Error(err)
			}
		}
	}
}

/*
	HandleConnection handles the received packets; it checks the flag of the
	packet and schedules a corresponding process function and returns an error.
 */
func (p *ProviderServer) HandleConnection(conn net.Conn, errs chan<- error) {

	buff := make([]byte, 1024)
	reqLen, err := conn.Read(buff)
	defer conn.Close()

	if err != nil {
		errs <- err
	}

	var packet config.GeneralPacket
	err = proto.Unmarshal(buff[:reqLen], &packet)
	if err != nil {
		errs <- err
	}

	switch packet.Flag {
	case ASSIGNE_FLAG:
		err = p.HandleAssignRequest(packet.Data)
		if err != nil {
			errs <- err
		}
	case COMM_FLAG:
		err = p.ReceivedPacket(packet.Data)
		if err != nil {
			errs <- err
		}
	case PULL_FLAG:
		err = p.HandlePullRequest(packet.Data)
		if err != nil{
			errs <- err
		}
	default:
		log.WithFields(log.Fields{"id" : p.Id}).Info("Packet flag not recognised. Packet dropped")
		errs <- nil
	}
	errs <- nil
}

/*
	RegisterNewClient generates a fresh authentication token and saves it together with client's public configuration data
	in the list of all registered clients. After the client is registered the function creates an inbox directory
	for the client's inbox, in which clients messages will be stored.
 */

func (p *ProviderServer) RegisterNewClient(clientBytes []byte) ([]byte, string, error){
	var clientConf config.ClientConfig
	err := proto.Unmarshal(clientBytes, &clientConf)
	if err != nil{
		return nil, "", err
	}

	token := helpers.MD5Hash([]byte("TMP_Token" + clientConf.Id))
	record := ClientRecord{Id: clientConf.Id, Host: clientConf.Host, Port: clientConf.Port, PubKey: clientConf.PubKey, Token: token}
	p.assignedClients[clientConf.Id] = record
	address := clientConf.Host + ":" + clientConf.Port

	path := fmt.Sprintf("./inboxes/%s", clientConf.Id)
	exists, err := helpers.DirExists(path)
	if err != nil{
		return nil, "", err
	}
	if exists == false {
		if err := os.MkdirAll(path, 0775); err != nil {
			return nil, "", err
		}
	}

	return token, address, nil
}

/*
	Function is responsible for handling the registration request from the client.
	it registers the client in the list of all registered clients and send
	an authentication token back to the client.
*/
func (p *ProviderServer) HandleAssignRequest(packet []byte) error {
	log.WithFields(log.Fields{"id" : p.Id}).Info("Received assign request from the client")

	token, adr, err := p.RegisterNewClient(packet)
	if err != nil {
		return err
	}

	tokenBytes, err := config.WrapWithFlag(TOKEN_FLAG, token)
	if err != nil {
		return err
	}
	err = p.Send(tokenBytes, adr)
	if err != nil {
		return err
	}
	return nil
}

/*
	Function is responsible for handling the pull request received from the client.
	It first authenticates the client, by checking if the received token is valid.
	If yes, the function triggers the function for checking client's inbox
	and sending buffered messages. Otherwise, an error is returned.
*/
func (p *ProviderServer) HandlePullRequest(rqsBytes []byte) error {
	var request config.PullRequest
	err := proto.Unmarshal(rqsBytes, &request)
	if err != nil {
		return err
	}

	log.WithFields(log.Fields{"id" : p.Id}).Info(fmt.Sprintf(" Processing pull request: %s %s", request.ClientId, string(request.Token)))

	if p.AuthenticateUser(request.ClientId, request.Token) == true{
		signal, err := p.FetchMessages(request.ClientId)
		if err != nil {
			return err
		}
		switch signal{
		case "NI":
			log.WithFields(log.Fields{"id" : p.Id}).Info("Inbox does not exist. Sending signal to client.")
		case "EI":
			log.WithFields(log.Fields{"id" : p.Id}).Info("Inbox is empty. Sending info to the client.")
		case "SI":
			log.WithFields(log.Fields{"id" : p.Id}).Info("All messages from the inbox succesfuly sent to the client.")
		}
	} else {
		log.WithFields(log.Fields{"id" : p.Id}).Warning("Authentication went wrong")
		return errors.New("authentication went wrong")
	}
	return nil
}

/*
	AuthenticateUser compares the authentication token received from the client with
	the one stored by the provider. If tokens are the same, it returns true
	and false otherwise.
*/
func (p *ProviderServer) AuthenticateUser(clientId string, clientToken []byte) bool{

	if bytes.Compare(p.assignedClients[clientId].Token, clientToken) == 0 {
		return true
	}
	return false
}

/*
	FetchMessages fetches messages from the requested inbox.
	FetchMessages checks whether an inbox exists and if it contains
	stored messages. If inbox contains any stored messages, all of them
	are send to the client one by one. FetchMessages returns a code
	signaling whether (NI) inbox does not exist, (EI) inbox is empty,
	(SI) messages were send to the client; and an error.
*/
func (p *ProviderServer) FetchMessages(clientId string) (string, error){

	path := fmt.Sprintf("./inboxes/%s", clientId)
	exist, err := helpers.DirExists(path)
	if err != nil{
		return "", err
	}
	if exist == false{
		return "NI", nil
	}
	files, err := ioutil.ReadDir(path)
	if err != nil{
		return "", err
	}
	if len(files) == 0 {
		return "EI", nil
	}

	for _, f := range files {
		dat, err := ioutil.ReadFile(path + "/" + f.Name())
		if err !=nil {
			return "", err
		}

		address := p.assignedClients[clientId].Host + ":" + p.assignedClients[clientId].Port
		log.WithFields(log.Fields{"id" : p.Id}).Info(fmt.Sprintf("Found stored message for address %s", address))
		msgBytes, err := config.WrapWithFlag(COMM_FLAG, dat)
		if err !=nil {
			return "", err
		}
		err = p.Send(msgBytes, address)
		if err !=nil {
			return "", err
		}
	}
	return "SI", nil
}

/*
	StoreMessage saves the given message in the inbox defined by the given id.
	If the inbox address does not exist or writing into the inbox was unsuccessful
	the function returns an error
*/
func (p *ProviderServer) StoreMessage(message []byte, inboxId string, messageId string) error {
	path := fmt.Sprintf("./inboxes/%s", inboxId)
	fileName := path + "/" + messageId + ".txt"

	file, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.Write(message)
	if err != nil {
		return err
	}

	log.WithFields(log.Fields{"id" : p.Id}).Info(fmt.Sprintf(" Stored message for %s", inboxId))
	return nil
}

/*
	NewProviderServer constructs a new provider object.
	Function returns a new provider object and an error.
*/
func NewProviderServer(id string, host string, port string, pubKey []byte, prvKey []byte, pkiPath string) (*ProviderServer, error) {
	node := node.Mix{Id: id, PubKey: pubKey, PrvKey: prvKey}
	providerServer := ProviderServer{Host: host, Port: port, Mix: node, listener: nil}
	providerServer.Config = config.MixConfig{Id: providerServer.Id, Host: providerServer.Host, Port: providerServer.Port, PubKey: providerServer.PubKey}
	providerServer.assignedClients = make(map[string]ClientRecord)

	configBytes, err := proto.Marshal(&providerServer.Config)
	if err != nil{
		return nil, err
	}
	err = helpers.AddToDatabase(pkiPath, "Pki", providerServer.Id, "Provider", configBytes)
	if err != nil{
		return nil, err
	}

	addr, err := helpers.ResolveTCPAddress(providerServer.Host, providerServer.Port)

	if err != nil {
		return nil, err
	}
	providerServer.listener, err = net.ListenTCP("tcp", addr)

	if err != nil {
		return nil, err
	}

	return &providerServer, nil
}