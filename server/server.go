package server
//package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/rpc"
	"runtime"
	"sync"

	. "afs/lib" //types and utils

	"github.com/dedis/crypto/abstract"
	"github.com/dedis/crypto/edwards"
	"github.com/dedis/crypto/proof"
	"github.com/dedis/crypto/shuffle"
)

var TotalClients = 0

//any variable/func with 2: similar object as s-c but only s-s
type Server struct {
	addr            string //this server
	port            int
	id              int
	servers         []string //other servers
	rpcServers      []*rpc.Client
	regLock         []*sync.Mutex //registration mutex
	regChan         chan bool
	regDone         chan bool
	connectDone     chan bool
	secretLock      *sync.Mutex
	pkLock          *sync.Mutex //avoid data race in go

	//crypto
	suite           abstract.Suite
	g               abstract.Group
	rand            abstract.Cipher
	sk              abstract.Secret //secret and public elgamal key
	pk              abstract.Point
	pks             []abstract.Point //all servers pks
	nextPk          abstract.Point
	ephSecret       abstract.Secret

	//clients
	clientMap       map[int]int //maps clients to dedicated server
	numClients      int //#clients connect here
	totalClients    int //total number of clients (sum of all servers)
	masks           [][]byte //clients' masks for PIR
	secrets         [][]byte //shared secret used to xor

	//all rounds
	rounds          []*Round
}

//per round variables
type Round struct {
	allBlocks       []Block //all blocks store on this server

	//requesting
	requestsChan    []chan Request
	reqHashes       [][]byte
	reqHashesRdy    []chan bool

	//uploading
	ublockChan2     chan UpBlock
	shuffleChan     chan []UpBlock //collect all uploads together

	//downloading
	upHashes        [][]byte
	dblocksChan     chan []Block
	blocksRdy       []chan bool
	upHashesRdy     []chan bool
	blocks          [](map[int][]Block) //keep track of blocks mapped to this server
	xorsChan        []map[int](chan Block)
	maskChan        chan []byte
}

///////////////////////////////
//Initial Setup
//////////////////////////////

func NewServer(addr string, port int, id int, servers []string) *Server {
	suite := edwards.NewAES128SHA256Ed25519(false)
	rand := suite.Cipher(abstract.RandomKey)
	sk := suite.Secret().Pick(rand)
	pk := suite.Point().Mul(nil, sk)
	ephSecret := suite.Secret().Pick(rand)

	rounds := make([]*Round, MaxRounds)

	for i := range rounds {
		r := Round{
			allBlocks:      nil,

			requestsChan:   nil,
			reqHashes:      nil,
			reqHashesRdy:   nil,

			ublockChan2:    nil,
			shuffleChan:    make(chan []UpBlock),

			upHashes:       nil,
			dblocksChan:    make(chan []Block),
			blocksRdy:      nil,
			upHashesRdy:    nil,
			xorsChan:       make([]map[int](chan Block), len(servers)),
		}
		rounds[i] = &r
	}

	s := Server{
		addr:           addr,
		port:           port,
		id:             id,
		servers:        servers,
		regLock:        []*sync.Mutex{new(sync.Mutex), new(sync.Mutex)},
		regChan:        make(chan bool, TotalClients),
		regDone:        make(chan bool),
		secretLock:     new(sync.Mutex),
		pkLock:         new(sync.Mutex),
		connectDone:    make(chan bool),

		suite:          suite,
		g:              suite,
		rand:           rand,
		sk:             sk,
		pk:             pk,
		pks:            make([]abstract.Point, len(servers)),
		ephSecret:      ephSecret,

		clientMap:      make(map[int]int),
		numClients:     0,
		totalClients:   0,
		masks:          nil,
		secrets:        nil,

		rounds:         rounds,
	}

	return &s
}


/////////////////////////////////
//Helpers
////////////////////////////////

func (s *Server) runHandlers() {
	<-s.connectDone
	<-s.regDone
	runHandler(s.handleRequests)
	runHandler(s.gatherUploads)
	runHandler(s.shuffleUploads)
	runHandler(s.handleResponses)
}

func (s *Server) handleRequests(round int) {
	allRequests := make([][][]byte, s.totalClients)

	var wg sync.WaitGroup
	for i := range allRequests {
		wg.Add(1)
		go func (i int) {
			defer wg.Done()
			req := <-s.rounds[round].requestsChan[i]
			allRequests[i] = req.Hash
		} (i)
	}
	wg.Wait()

	s.rounds[round].reqHashes = XorsDC(allRequests)
	for i := range s.rounds[round].reqHashesRdy {
		if s.clientMap[i] != s.id {
			continue
		}
		go func(i int, rount int) {
			s.rounds[round].reqHashesRdy[i] <- true
		} (i, round)
	}
}

func (s *Server) handleResponses(round int) {
	allBlocks := <-s.rounds[round].dblocksChan
	for i := 0; i < s.totalClients; i++ {
		if s.clientMap[i] == s.id {
			continue
		}
		//if it doesnt belong to me, xor things and send it over
		go func(i int, sid int) {
			res := ComputeResponse(allBlocks, s.masks[i], s.secrets[i])
			//fmt.Println(s.id, "mask for", i, s.masks[i])
			rand := s.suite.Cipher(s.secrets[i])
			rand.Read(s.secrets[i])
			rand = s.suite.Cipher(s.masks[i])
			rand.Read(s.masks[i])
			fmt.Println(s.id, round, "mask", i, s.masks[i])
			cb := ClientBlock {
				CId: i,
				SId: s.id,
				Block: Block {
					Block: res,
					Round: round,
				},
			}
			err := s.rpcServers[sid].Call("Server.PutClientBlock", cb, nil)
			if err != nil {
				log.Fatal("Couldn't put block: ", err)
			}
		} (i, s.clientMap[i])
	}

	//store it on this server as well
	s.rounds[round].allBlocks = allBlocks

	for i := range s.rounds[round].blocksRdy {
		if s.clientMap[i] != s.id {
			continue
		}
		go func(i int, round int) {
			s.rounds[round].blocksRdy[i] <- true
		} (i, round)
	}
}

func (s *Server) gatherUploads(round int) {
	allUploads := make([]UpBlock, s.totalClients)
	for i := 0; i < s.totalClients; i++ {
		allUploads[i] = <-s.rounds[round].ublockChan2
	}
	//fmt.Println(s.id, "done gathering", round)
	s.rounds[round].shuffleChan <- allUploads
}

func (s *Server) shuffleUploads(round int) {
	allUploads := <-s.rounds[round].shuffleChan
	//fmt.Println(s.id, "shuffle start: ", round)

	//shuffle and reblind

	hashChunks := len(allUploads[0].HC1[0])
	// for _, upload := range allUploads {
	// 	if hashChunks != len(upload.HC1[0])  {
	// 		panic("Different chunk lengths")
	// 	}
	// }

	HXss := make([][][]abstract.Point, len(s.servers))
	HYss := make([][][]abstract.Point, len(s.servers))

	for i := range HXss {
		HXss[i] = make([][]abstract.Point, hashChunks)
		HYss[i] = make([][]abstract.Point, hashChunks)
		for j := range HXss[i] {
			HXss[i][j] = make([]abstract.Point, s.totalClients)
			HYss[i][j] = make([]abstract.Point, s.totalClients)
			for k := range HXss[i][j] {
				HXss[i][j][k] = UnmarshalPoint(s.suite, allUploads[k].HC1[i][j])
				HYss[i][j][k] = UnmarshalPoint(s.suite, allUploads[k].HC2[i][j])
			}
		}
	}

	DX := make([]abstract.Point, s.totalClients)
	DY := make([]abstract.Point, s.totalClients)
	for j := 0; j < s.totalClients; j++ {
		DX[j] = UnmarshalPoint(s.suite, allUploads[j].DH1)
		DY[j] = UnmarshalPoint(s.suite, allUploads[j].DH2)
	}

	//TODO: need to send ybar and proofs out out eventually
	pi := GeneratePI(s.totalClients, s.rand)

	HXbarss := make([][][]abstract.Point, len(s.servers))
	HYbarss := make([][][]abstract.Point, len(s.servers))
	Hdecss := make([][][]abstract.Point, len(s.servers))
	prfs := make([][][]byte, len(s.servers))

	ephKeys := make([]abstract.Point, s.totalClients)
	decBlocks := make([][]byte, s.totalClients)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		var shuffleWG sync.WaitGroup
		for i := range s.servers {
			if i >= s.id {
				shuffleWG.Add(1)
				go func(i int) {
					defer shuffleWG.Done()
					HXbarss[i], HYbarss[i], Hdecss[i], prfs[i] = s.shuffle(pi, HXss[i], HYss[i], hashChunks)
				} (i)
			}
		}
		shuffleWG.Wait()
	} ()
	go func() {
		defer wg.Done()
		var aesWG sync.WaitGroup
		for j := 0; j < s.totalClients; j++ {
			aesWG.Add(1)
			go func(j int) {
				defer aesWG.Done()
				ephKey := Decrypt(s.g, DX[pi[j]], DY[pi[j]], s.sk)
				key := MarshalPoint(s.g.Point().Mul(ephKey, s.ephSecret))
				//shuffle using pi
				decBlocks[j] = CounterAES(key, allUploads[pi[j]].BC)
				ephKeys[j] = ephKey
			} (j)
		}
		aesWG.Wait()
	} ()
	wg.Wait()



	if s.id == len(s.servers) - 1 {
		//last server to shuffle, broadcast
		hashes := make([][]byte, s.totalClients)
		blocks := make([]Block, s.totalClients)
		//TODO: check hashes
		for i := range blocks {
			hash := []byte{}
			for j := range Hdecss[s.id] {
				msg, err := Hdecss[s.id][j][i].Data()
				if err != nil {
					log.Fatal("Could not decrypt: ", err)
				}
				hash = append(hash, msg...)
			}
			hashes[i] = hash
			blocks[i] = Block {
				Hash:  hash,
				Block: decBlocks[i],
				Round: round,
			}
			// if round == 0 {
			//  	fmt.Println(round, "final block: ", decBlocks[i], hashes[i])
			// }
		}
		var wg sync.WaitGroup
		for _, rpcServer := range s.rpcServers {
			wg.Add(1)
			go func(rpcServer *rpc.Client) {
				defer wg.Done()
				err := rpcServer.Call("Server.PutUploadedBlocks", &blocks, nil)
				if err != nil {
					log.Fatal("Failed uploading shuffled and decoded blocks: ", err)
				}
			} (rpcServer)
		}
		wg.Wait()
	} else {
		for i := range allUploads {
			for j := range allUploads[i].HC1 {
				for k := range allUploads[i].HC1[j] {
					if j <= s.id {
						allUploads[i].HC1[j][k] = []byte{}
						allUploads[i].HC2[j][k] = []byte{}
					} else {
						allUploads[i].HC1[j][k] = MarshalPoint(HXbarss[j][k][i])
						allUploads[i].HC2[j][k] = MarshalPoint(Hdecss[j][k][i])
					}
				}
			}
			dh1, dh2 := EncryptPoint(s.g, ephKeys[i], s.pks[s.id+1])
			allUploads[i].DH1 = MarshalPoint(dh1)
			allUploads[i].DH2 = MarshalPoint(dh2)
			allUploads[i].BC = decBlocks[i]
		}
		err := s.rpcServers[s.id+1].Call("Server.ShuffleBlocks", allUploads, nil)
		if err != nil {
			log.Fatal("Failed requesting shuffle: ", err)
		}

	}
	//fmt.Println(s.id, "shuffle done: ", round)
}

func (s *Server) shuffle(pi []int, Xs [][]abstract.Point, Ys [][]abstract.Point, numChunks int) ([][]abstract.Point,
	[][]abstract.Point, [][]abstract.Point, [][]byte) {
	Xbars := make([][]abstract.Point, numChunks)
	Ybars := make([][]abstract.Point, numChunks)
	decs := make([][]abstract.Point, numChunks)
	provers := make([] proof.Prover, numChunks)
	prfs := make([][]byte, numChunks)
	pk := s.nextPk

	//do the shuffle, and blind using next server's keys
	//everyone shares the same group
	var wg sync.WaitGroup
	for i := range decs {
		wg.Add(1)
		go func (i int) {
			defer wg.Done()
			Xbars[i], Ybars[i], provers[i] = shuffle.Shuffle2(pi, s.g, nil, pk, Xs[i], Ys[i], s.rand)
			prf, err := proof.HashProve(s.suite, "PairShuffle", s.rand, provers[i])
			if err != nil {
				panic("Shuffle proof failed: " + err.Error())
			}
			prfs[i] = prf

			//decrypt a layer
			var decWG sync.WaitGroup
			decs[i] = make([]abstract.Point, s.totalClients)
			for j := 0; j < s.totalClients; j++ {
				decWG.Add(1)
				go func (i int, j int) {
					defer decWG.Done()
					c1 := Xbars[i][j]
					c2 := Ybars[i][j]
					decs[i][j] = Decrypt(s.g, c1, c2, s.sk)
				} (i, j)
			}
			decWG.Wait()
		} (i)
	}
	wg.Wait()

	return Xbars, Ybars, decs, prfs
}

/////////////////////////////////
//Registration and Setup
////////////////////////////////
//register the client here, and notify the server it will be talking to
//TODO: should check for duplicate clients, just in case..
func (s *Server) Register(serverId int, clientId *int) error {
	s.regLock[0].Lock()
	*clientId = s.totalClients
	client := &ClientRegistration{
		ServerId: serverId,
		Id: *clientId,
	}
	s.totalClients++
	for _, rpcServer := range s.rpcServers {
		err := rpcServer.Call("Server.Register2", client, nil)
		if err != nil {
			log.Fatal(fmt.Sprintf("Cannot connect to %d: ", serverId), err)
		}
	}
	if s.totalClients == TotalClients {
		s.registerDone()
	}
	fmt.Println("Registered", *clientId)
	s.regLock[0].Unlock()
	return nil
}

//called to increment total number of clients
func (s *Server) Register2(client *ClientRegistration, _ *int) error {
	s.regLock[1].Lock()
	s.clientMap[client.Id] = client.ServerId
	s.regLock[1].Unlock()
	return nil
}

func (s *Server) registerDone() {
	for _, rpcServer := range s.rpcServers {
		err := rpcServer.Call("Server.RegisterDone2", s.totalClients, nil)
		if err != nil {
			log.Fatal("Cannot update num clients")
		}
	}

	for i := 0; i < s.totalClients; i++ {
		s.regChan <- true
	}
}

func (s *Server) RegisterDone2(numClients int, _ *int) error {
	s.totalClients = numClients

	s.masks = make([][]byte, numClients)
	s.secrets = make([][]byte, numClients)

	for i := range s.masks {
		s.masks[i] = make([]byte, SecretSize)
		s.secrets[i] = make([]byte, SecretSize)
	}

	for r := range s.rounds {

		for i := 0; i < len(s.servers); i++ {
			s.rounds[r].xorsChan[i] = make(map[int](chan Block))
			for j := 0; j < numClients; j++ {
				s.rounds[r].xorsChan[i][j] = make(chan Block)
			}
		}

		s.rounds[r].requestsChan = make([]chan Request, numClients)

		for i := range s.rounds[r].requestsChan {
			s.rounds[r].requestsChan[i] = make(chan Request)
		}

		s.rounds[r].upHashes = make([][]byte, numClients)

		s.rounds[r].blocksRdy = make([]chan bool, numClients)
		s.rounds[r].upHashesRdy = make([]chan bool, numClients)
		s.rounds[r].reqHashesRdy = make([]chan bool, numClients)
		for i := range s.rounds[r].blocksRdy {
			s.rounds[r].blocksRdy[i] = make(chan bool)
			s.rounds[r].upHashesRdy[i] = make(chan bool)
			s.rounds[r].reqHashesRdy[i] = make(chan bool)
		}

		s.rounds[r].ublockChan2 = make(chan UpBlock, numClients-1)
	}
	s.regDone <- true
	fmt.Println(s.id, "Register done")
	return nil
}

func (s *Server) connectServers() {
	rpcServers := make([]*rpc.Client, len(s.servers))
	for i := range rpcServers {
		var rpcServer *rpc.Client
		var err error = errors.New("")
		for ; err != nil ; {
			if i == s.id {
				//make a local rpc
				addr := fmt.Sprintf("127.0.0.1:%d", s.port)
				rpcServer, err = rpc.Dial("tcp", addr)
			} else {
				rpcServer, err = rpc.Dial("tcp", s.servers[i])
			}
			rpcServers[i] = rpcServer
		}
	}

	var wg sync.WaitGroup
	for i, rpcServer := range rpcServers {
		wg.Add(1)
		go func (i int, rpcServer *rpc.Client) {
			defer wg.Done()
			pk := make([]byte, SecretSize)
			err := rpcServer.Call("Server.GetPK", 0, &pk)
			if err != nil {
				log.Fatal("Couldn't get server's pk: ", err)
			}
			s.pks[i] = UnmarshalPoint(s.suite, pk)
		} (i, rpcServer)
	}
	wg.Wait()
	if s.id != len(s.servers)-1 {
		s.nextPk = s.pks[s.id]
		for i := s.id+1; i < len(s.servers); i++ {
			s.nextPk = s.g.Point().Add(s.nextPk, s.pks[i])
		}
	} else {
		s.nextPk = s.pk
	}
	s.rpcServers = rpcServers
	s.connectDone <- true
}

func (s *Server) GetNumClients(_ int, num *int) error {
	<-s.regChan
	*num = s.totalClients
	return nil
}

func (s *Server) GetPK(_ int, pk *[]byte) error {
	s.pkLock.Lock()
	*pk = MarshalPoint(s.pk)
	s.pkLock.Unlock()
	return nil
}

func (s *Server) shareSecret(clientPublic abstract.Point) (abstract.Point, abstract.Point) {
	s.secretLock.Lock()
	gen := s.g.Point().Base()
	secret := s.g.Secret().Pick(s.rand)
	public := s.g.Point().Mul(gen, secret)
	sharedSecret := s.g.Point().Mul(clientPublic, secret)
	s.secretLock.Unlock()
	return public, sharedSecret
}

func (s *Server) ShareMask(clientDH *ClientDH, serverPub *[]byte) error {
	pub, shared := s.shareSecret(UnmarshalPoint(s.suite, clientDH.Public))
	s.masks[clientDH.Id] = MarshalPoint(shared)
	fmt.Println(s.id, "mask", clientDH.Id, MarshalPoint(shared))
	// s.masks[clientDH.Id] = make([]byte, len(MarshalPoint(shared)))
	// s.masks[clientDH.Id][clientDH.Id] = 1
	*serverPub = MarshalPoint(pub)
	return nil
}

func (s *Server) ShareSecret(clientDH *ClientDH, serverPub *[]byte) error {
	pub, shared := s.shareSecret(UnmarshalPoint(s.suite, clientDH.Public))
	//s.secrets[clientDH.Id] = MarshalPoint(shared)
	s.secrets[clientDH.Id] = make([]byte, len(MarshalPoint(shared)))
	*serverPub = MarshalPoint(pub)
	return nil
}

func (s *Server) GetEphKey(_ int, serverPub *[]byte) error {
	pub := s.g.Point().Mul(s.g.Point().Base(), s.ephSecret)
	*serverPub = MarshalPoint(pub)
	return nil
}

/////////////////////////////////
//Request
////////////////////////////////
func (s *Server) RequestBlock(cr *ClientRequest, _ *int) error {
	var wg sync.WaitGroup
	for i, rpcServer := range s.rpcServers {
		wg.Add(1)
		go func (i int, rpcServer *rpc.Client) {
			defer wg.Done()
			err := rpcServer.Call("Server.ShareRequest", cr, nil)
			if err != nil {
				log.Fatal("Couldn't share request: ", err)
			}
		} (i, rpcServer)
	}
	wg.Wait()
	return nil
}

func (s *Server) ShareRequest(cr *ClientRequest, _ *int) error {
	round := cr.Request.Round % MaxRounds
	s.rounds[round].requestsChan[cr.Id] <- cr.Request
	return nil
}

func (s *Server) GetReqHashes(args *RequestArg, hashes *[][]byte) error {
	round := args.Round % MaxRounds
	<-s.rounds[round].reqHashesRdy[args.Id]
	*hashes = s.rounds[round].reqHashes
	return nil
}

/////////////////////////////////
//Upload
////////////////////////////////
func (s *Server) UploadBlock(block *UpBlock, _ *int) error {
	err := s.rpcServers[0].Call("Server.UploadBlock2", block, nil)
	if err != nil {
		log.Fatal("Couldn't send block to first server: ", err)
	}
	return nil
}

func (s *Server) UploadBlock2(block *UpBlock, _*int) error {
	round := block.Round % MaxRounds
	s.rounds[round].ublockChan2 <- *block
	//fmt.Println("put ublockchan2", round)
	return nil
}

func (s *Server) ShuffleBlocks(blocks *[]UpBlock, _*int) error {
	round := (*blocks)[0].Round % MaxRounds
	s.rounds[round].shuffleChan <- *blocks
	return nil
}


/////////////////////////////////
//Download
////////////////////////////////
func (s *Server) GetUpHashes(args *RequestArg, hashes *[][]byte) error {
	round := args.Round % MaxRounds
	<-s.rounds[round].upHashesRdy[args.Id]
	*hashes = s.rounds[round].upHashes
	return nil
}

func (s *Server) GetResponse(cmask ClientMask, response *[]byte) error {
	otherBlocks := make([][]byte, len(s.servers))
	var wg sync.WaitGroup
	round := cmask.Round % MaxRounds
	for i := range otherBlocks {
		if i == s.id {
			otherBlocks[i] = make([]byte, BlockSize)
		} else {
			wg.Add(1)
			go func(i int, cmask ClientMask) {
				defer wg.Done()
				curBlock := <-s.rounds[round].xorsChan[i][cmask.Id]
				//fmt.Println(s.id, "mask for", cmask.Id, cmask.Mask)
				otherBlocks[i] = curBlock.Block
			} (i, cmask)
		}
	}
	wg.Wait()
	<-s.rounds[round].blocksRdy[cmask.Id]
	r := ComputeResponse(s.rounds[round].allBlocks, cmask.Mask, s.secrets[cmask.Id])
	rand := s.suite.Cipher(s.secrets[cmask.Id])
	rand.Read(s.secrets[cmask.Id])
	Xor(Xors(otherBlocks), r)
	*response = r
	return nil
}

//used to push response for particular client
func (s *Server) PutClientBlock(cblock ClientBlock, _ *int) error {
	block := cblock.Block
	round := block.Round % MaxRounds
	s.rounds[round].xorsChan[cblock.SId][cblock.CId] <- block
	return nil
}

//used to push the uploaded blocks from the final shuffle server
func (s *Server) PutUploadedBlocks(blocks *[]Block, _ *int) error {
	round := (*blocks)[0].Round % MaxRounds
	for i := range *blocks {
		h := s.suite.Hash()
		h.Write((*blocks)[i].Block)
		hash := h.Sum(nil)
		s.rounds[round].upHashes[i] = hash
		//fmt.Println("block", (*blocks)[i].Block, s.rounds[round].upHashes[i])
		if !SliceEquals(hash, (*blocks)[i].Hash) {
			log.Fatal("Hash mismatch!")
		}
	}

	for i := range s.rounds[round].upHashesRdy {
		if s.clientMap[i] != s.id {
			continue
		}
		go func(i int, round int) {
			s.rounds[round].upHashesRdy[i] <- true
		} (i, round)
	}

	s.rounds[round].dblocksChan <- *blocks
	return nil
}

/////////////////////////////////
//Misc
////////////////////////////////
//used for the local test function to start the server
func (s *Server) MainLoop(_ int, _ *int) error {
	rpcServer := rpc.NewServer()
	rpcServer.Register(s)
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		panic("Cannot starting listening to the port")
	}
	go rpcServer.Accept(l)

	go s.runHandlers()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.connectServers()
	} ()
	wg.Wait()

	return nil
}

func (s *Server) Masks() [][]byte {
	return s.masks
}

func (s *Server) Secrets() [][]byte {
	return s.secrets
}

func runHandler(f func(int)) {
	for r := 0; r < MaxRounds; r++ {
		go func (r int) {
			for {
				f(r)
			}
		} (r)
	}
}

func SetTotalClients(n int) {
	TotalClients = n
}

/////////////////////////////////
//MAIN
/////////////////////////////////
func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	var id *int = flag.Int("i", 0, "id [num]")
	var servers *string = flag.String("s", "", "servers [file]")
	var numClients *int = flag.Int("n", 0, "num clients [num]")
	flag.Parse()

	ss := ParseServerList(*servers)

	SetTotalClients(*numClients)

	s := NewServer(ss[*id], ServerPort + *id, *id, ss)

	rpcServer := rpc.NewServer()
	rpcServer.Register(s)
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		panic("Cannot starting listening to the port")
	}
	go s.connectServers()
	fmt.Println("Starting server", *id)
	s.runHandlers()
	rpcServer.Accept(l)
}

