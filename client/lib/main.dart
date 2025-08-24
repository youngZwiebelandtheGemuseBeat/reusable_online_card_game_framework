import 'dart:math' as math;
import 'package:flutter/material.dart';
import 'ws_service.dart';

// Optional: allow overriding WS via --dart-define=WS_URL=ws://host:port/ws
const _WS_URL = String.fromEnvironment('WS_URL', defaultValue: '');

void main() {
  WidgetsFlutterBinding.ensureInitialized();
  runApp(const AppRoot());
}

class AppRoot extends StatefulWidget {
  const AppRoot({super.key});
  @override
  State<AppRoot> createState() => _AppRootState();
}

class _AppRootState extends State<AppRoot> {
  final ws = WsService();
  final nameCtrl = TextEditingController();

  @override
  void initState() {
    super.initState();
    ws.connect(explicitUrl: _WS_URL.isEmpty ? null : _WS_URL);
  }

  @override
  void dispose() {
    ws.close();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'Lobby',
      theme: ThemeData(useMaterial3: true, colorSchemeSeed: Colors.blueGrey),
      home: Scaffold(appBar: AppBar(title: const Text('Lobby')), body: LobbyPage(ws: ws, nameCtrl: nameCtrl)),
    );
  }
}

// -------------------- Lobby --------------------

class LobbyPage extends StatefulWidget {
  final WsService ws;
  final TextEditingController nameCtrl;
  const LobbyPage({super.key, required this.ws, required this.nameCtrl});
  @override
  State<LobbyPage> createState() => _LobbyPageState();
}

class _LobbyPageState extends State<LobbyPage> {
  List<Map<String, dynamic>> rooms = [];
  String? createdRoomId;
  final roomCtrl = TextEditingController();

  @override
  void initState() {
    super.initState();
    widget.ws.messages.listen((m) {
      if (!mounted) return;
      if (m['t'] == 'rooms') {
        final list = (m['m']['list'] as List? ?? const [])
            .map((e) => (e as Map).map((k, v) => MapEntry(k.toString(), v)))
            .cast<Map<String, dynamic>>()
            .toList();
        setState(() => rooms = list);
      } else if (m['t'] == 'created') {
        setState(() => createdRoomId = (m['m']['room'] ?? '').toString());
      }
    });
  }

  void _applyName() {
    final s = widget.nameCtrl.text.trim();
    if (s.isEmpty) return;
    widget.ws.send({"t":"set_name","m":{"name": s}});
  }

  void _create() => widget.ws.send({"t":"create_table","m":{"seats":3}});

  void _joinById(String id) {
    if (id.isEmpty) return;
    widget.ws.send({"t":"join_table","m":{"room": id}});
    Navigator.of(context).push(MaterialPageRoute(builder: (_) => TablePage(ws: widget.ws, roomId: id)));
  }

  void _joinFromList(String id) {
    widget.ws.send({"t":"join_table","m":{"room": id}});
    Navigator.of(context).push(MaterialPageRoute(builder: (_) => TablePage(ws: widget.ws, roomId: id)));
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.all(12),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Row(children: [
          SizedBox(width: 260, child: TextField(controller: widget.nameCtrl, decoration: const InputDecoration(labelText: 'Your display name (optional)', isDense: true), onSubmitted: (_) => _applyName())),
          const SizedBox(width: 8),
          OutlinedButton(onPressed: _applyName, child: const Text('Set name')),
        ]),
        const SizedBox(height: 12),
        Wrap(spacing: 8, runSpacing: 8, children: [
          FilledButton(onPressed: _create, child: const Text('Create table (3)')),
          SizedBox(width: 280, child: TextField(controller: roomCtrl, decoration: const InputDecoration(labelText: 'Room ID', hintText: 'Paste to join', isDense: true))),
          OutlinedButton(onPressed: () => _joinById(roomCtrl.text.trim()), child: const Text('Join by ID')),
        ]),
        const SizedBox(height: 16),
        const Text('Open tables:', style: TextStyle(fontWeight: FontWeight.bold)),
        const SizedBox(height: 8),
        Expanded(
          child: rooms.isEmpty
              ? const Center(child: Text('No tables yet. Create one!'))
              : ListView.separated(
                  itemCount: rooms.length,
                  separatorBuilder: (_, __) => const Divider(),
                  itemBuilder: (_, i) {
                    final r = rooms[i];
                    final id = (r['id'] ?? '').toString();
                    final seats = ((r['seats'] as num?) ?? 0).toInt();
                    final occ = ((r['occupied'] as num?) ?? 0).toInt();
                    final started = (r['started'] ?? false) as bool;
                    final missing = seats - occ;
                    final full = occ >= seats;

                    return ListTile(
                      title: Text('Room $id'),
                      subtitle: Text('Seats: $seats  •  Occupied: $occ  •  ${started ? 'In progress' : 'Waiting'}'),
                      trailing: full
                          ? const Text('Full', style: TextStyle(color: Colors.red))
                          : FilledButton(onPressed: () => _joinFromList(id), child: Text(missing > 0 ? 'Join ($missing free)' : 'Join')),
                    );
                  },
                ),
        ),
        if (createdRoomId != null) ...[
          const Divider(),
          Text('Created room: $createdRoomId'),
          Row(children: [
            OutlinedButton(onPressed: () => _joinFromList(createdRoomId!), child: const Text('Open table')),
          ]),
        ]
      ]),
    );
  }
}

// -------------------- Table --------------------

class TablePage extends StatefulWidget {
  final WsService ws;
  final String roomId;
  const TablePage({super.key, required this.ws, required this.roomId});
  @override
  State<TablePage> createState() => _TablePageState();
}

class _TablePageState extends State<TablePage> {
  int? seat;
  int? turn;
  int? dealer;
  int? firstBidder;
  String? phase;           // start | cut | bidding | pick_trump | exchange | play
  int? actor;              // whose turn to act (start/bidding/exchange)
  int? bestBid;
  int? bestBy;
  List<int> passed = [];
  bool roundDouble = false;

  Map<String, dynamic>? cutPeek;

  String? trump;
  String? lead;
  bool handOver = false;
  List<dynamic> hand = [];
  List<Map<String, dynamic>> trick = [];
  List<String> names = [];
  List<int> counts = [];
  List<int> stayed = [];

  int talon = 0;
  int swamp = 0;
  int exchangeMax = 3;

  final Set<int> _sel = {};

  final chatCtrl = TextEditingController();
  final List<String> chat = [];

  @override
  void initState() {
    super.initState();
    widget.ws.messages.listen((m) {
      if (!mounted) return;
      switch (m['t']) {
        case 'state':
          if ((m['m']?['room'] ?? '') == widget.roomId) {
            setState(() {
              seat = (m['m']['seat'] as num?)?.toInt();
              phase = m['m']['phase'] as String?;
              actor = (m['m']['actor'] as num?)?.toInt();
              dealer = (m['m']['dealer'] as num?)?.toInt();
              firstBidder = (m['m']['firstBidder'] as num?)?.toInt();

              bestBid = (m['m']['bestBid'] as num?)?.toInt();
              bestBy = (m['m']['bestBy'] as num?)?.toInt();
              passed = ((m['m']['passed'] as List?) ?? const []).map((e) => (e as num).toInt()).toList();
              roundDouble = (m['m']['roundDouble'] ?? false) as bool;

              final cp = m['m']['cutPeek'];
              cutPeek = (cp is Map) ? Map<String, dynamic>.from(cp.map((k, v) => MapEntry(k.toString(), v))) : null;

              turn = (m['m']['turn'] as num?)?.toInt();
              trump = m['m']['trump'] as String?;
              lead  = m['m']['lead'] as String?;
              handOver = (m['m']['handOver'] ?? false) as bool;
              hand = List<dynamic>.from(m['m']['you'] as List? ?? const []);
              trick = ((m['m']['trick'] as List?) ?? const []).map((e) => Map<String, dynamic>.from((e as Map).map((k, v) => MapEntry(k.toString(), v)))).toList();
              names = ((m['m']['names'] as List?) ?? const []).map((e) => (e ?? '').toString()).toList();
              counts = ((m['m']['counts'] as List?) ?? const []).map((e) => (e as num).toInt()).toList();

              stayed = ((m['m']['stayed'] as List?) ?? const []).map((e) => (e as num).toInt()).toList();
              talon = ((m['m']['talon'] as num?) ?? 0).toInt();
              swamp = ((m['m']['swamp'] as num?) ?? 0).toInt();
              exchangeMax = ((m['m']['exchangeMax'] as num?) ?? 3).toInt();

              if (phase != 'exchange' || seat != actor) {
                _sel.clear();
              }
            });
          }
          break;
        case 'chat':
          if ((m['m']?['room'] ?? '') == widget.roomId) {
            final fromName = (m['m']['from_name'] ?? '').toString();
            final from = fromName.isNotEmpty ? fromName : (m['m']['from'] ?? 'player').toString();
            final text = (m['m']['text'] ?? '').toString();
            setState(() => chat.add('[$from] $text'));
          }
          break;
      }
    });
  }

  void _leave() {
    widget.ws.send({"t":"leave_table","m":{"room": widget.roomId}});
    Navigator.of(context).pop();
  }

  void _newHand() => widget.ws.send({"t":"new_hand","m":{"room": widget.roomId}});
  void _sendChat() {
    final t = chatCtrl.text.trim();
    if (t.isEmpty) return;
    widget.ws.send({"t":"chat","m":{"room": widget.roomId, "text": t}});
    chatCtrl.clear();
  }

  // actions
  void _startChoice(String choice) => widget.ws.send({"t":"start_choice","m":{"room": widget.roomId, "seat": seat, "choice": choice}}); // 'cut' | 'knock'
  void _cutProceed() => widget.ws.send({"t":"cut_proceed","m":{"room": widget.roomId, "seat": seat}});
  void _pass() => widget.ws.send({"t":"pass","m":{"room": widget.roomId, "seat": seat}});
  void _bid(int n) => widget.ws.send({"t":"bid","m":{"room": widget.roomId, "seat": seat, "bid": n}});
  void _pickTrump(String t) => widget.ws.send({"t":"pick_trump","m":{"room": widget.roomId, "seat": seat, "trump": t}});

  void _toggleSel(int i) {
    if (phase != 'exchange' || seat != actor) return;
    if (_sel.contains(i)) {
      setState(() => _sel.remove(i));
    } else {
      if (_sel.length < exchangeMax) {
        setState(() => _sel.add(i));
      }
    }
  }
  void _stayHome() {
    widget.ws.send({"t":"stay_home","m":{"room": widget.roomId, "seat": seat}});
  }
  void _exchangeSelected() {
    final list = _sel.toList()..sort();
    final cards = <Map<String, String>>[];
    for (final i in list) {
      if (i >= 0 && i < hand.length) {
        final c = hand[i] as Map;
        final suit = (c['Suit'] ?? c['suit'] ?? '').toString();
        final rank = (c['Rank'] ?? c['rank'] ?? '').toString();
        cards.add({"Suit": suit, "Rank": rank});
      }
    }
    if (cards.isNotEmpty && cards.length <= exchangeMax) {
      widget.ws.send({"t":"exchange","m":{"room": widget.roomId, "seat": seat, "cards": cards}});
      setState(() => _sel.clear());
    }
  }
  void _exchangeDoneNoSwap() {
    widget.ws.send({"t":"exchange_done","m":{"room": widget.roomId, "seat": seat}});
  }

  bool _stayedSeat(int s) => stayed.contains(s);

  @override
  Widget build(BuildContext context) {
    final myPlayTurn = phase == 'play' && seat != null && turn != null && seat == turn;
    final seatsCount = counts.isNotEmpty ? counts.length : math.max(names.length, 0);

    final youAreActor = (seat != null && actor != null && seat == actor);
    final declarer = bestBy;
    final canStayHome = (trump != 'clubs') && (seat != declarer);

    return Scaffold(
      appBar: AppBar(
        title: Text('Table — ${widget.roomId}'),
        leading: IconButton(icon: const Icon(Icons.arrow_back), onPressed: _leave),
        actions: [
          if (handOver) Padding(padding: const EdgeInsets.symmetric(horizontal: 8), child: FilledButton(onPressed: _newHand, child: const Text('New hand'))),
        ],
      ),
      body: Padding(
        padding: const EdgeInsets.all(12),
        child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
          Text('Dealer: ${dealer ?? "-"}  |  First bidder: ${firstBidder ?? "-"}  |  Phase: ${phase ?? "-"}'),
          Text('Seat: ${seat ?? "-"}  |  Turn: ${turn ?? "-"}  |  Trump: ${trump ?? "-"}  |  Lead: ${lead ?? "-"}  |  Double: ${roundDouble ? "yes" : "no"}'),
          const SizedBox(height: 12),

          SizedBox(height: 280, child: PlayerRing(seats: seatsCount > 0 ? seatsCount : 3, names: names, counts: counts, youSeat: seat, turnSeat: turn, stayed: stayed)),

          const Divider(),

          // START
          if (phase == 'start') ...[
            Card(
              elevation: 0,
              color: Colors.orange.withOpacity(0.12),
              child: Padding(
                padding: const EdgeInsets.all(8.0),
                child: Row(children: [
                  Expanded(child: Text((seat == firstBidder) ? 'Start of hand — your choice' : 'Waiting for s$firstBidder')),
                  if (seat == firstBidder) ...[
                    OutlinedButton.icon(onPressed: () => _startChoice('cut'), icon: const Icon(Icons.content_cut), label: const Text('Cut')),
                    const SizedBox(width: 8),
                    FilledButton.icon(onPressed: () => _startChoice('knock'), icon: const Icon(Icons.back_hand), label: const Text('Knock (double)')),
                  ],
                ]),
              ),
            ),
            const SizedBox(height: 8),
          ],

          // CUT
          if (phase == 'cut') ...[
            if (seat == firstBidder && cutPeek != null)
              Card(
                elevation: 0,
                color: Colors.green.withOpacity(0.12),
                child: Padding(
                  padding: const EdgeInsets.all(8.0),
                  child: Row(children: [
                    const Icon(Icons.visibility),
                    const SizedBox(width: 8),
                    Text('Your cut — bottom card: ${(cutPeek!['rank'] ?? cutPeek!['Rank'])}-${(cutPeek!['suit'] ?? cutPeek!['Suit'])}'),
                    const Spacer(),
                    FilledButton.icon(onPressed: _cutProceed, icon: const Icon(Icons.play_arrow), label: const Text('Deal & start bidding')),
                  ]),
                ),
              )
            else
              Card(
                elevation: 0,
                color: Colors.green.withOpacity(0.06),
                child: const Padding(padding: EdgeInsets.all(8.0), child: Text('Cutter is inspecting the cut…')),
              ),
            const SizedBox(height: 8),
          ],

          // BIDDING / PICK_TRUMP
          if (phase == 'bidding' || phase == 'pick_trump') ...[
            if (phase == 'bidding')
              Card(
                elevation: 0,
                color: Colors.yellow.withOpacity(0.12),
                child: Padding(
                  padding: const EdgeInsets.all(8.0),
                  child: Row(children: [
                    Expanded(child: Text((actor != null && seat == actor) ? 'Your turn to bid' : 'Waiting for s${actor ?? "-"}')),
                    OutlinedButton(onPressed: (actor != null && seat == actor) ? _pass : null, child: const Text('Pass')),
                    const SizedBox(width: 8),
                    Wrap(spacing: 6, children: [
                      FilledButton(onPressed: (actor != null && seat == actor) ? () => _bid(1) : null, child: const Text('1 (♥)')),
                      FilledButton(onPressed: (actor != null && seat == actor) ? () => _bid(2) : null, child: const Text('2')),
                      FilledButton(onPressed: (actor != null && seat == actor) ? () => _bid(3) : null, child: const Text('3')),
                      FilledButton(onPressed: (actor != null && seat == actor) ? () => _bid(4) : null, child: const Text('4')),
                      FilledButton(onPressed: (actor != null && seat == actor) ? () => _bid(5) : null, child: const Text('5')),
                    ]),
                  ]),
                ),
              ),
            Padding(
              padding: const EdgeInsets.symmetric(vertical: 6),
              child: Text('Best bid: ${bestBid ?? 0}  by seat ${bestBy ?? "-"}  •  Passed: ${passed.map((e)=>"s$e").join(", ")}'),
            ),
            const Divider(),
            const Text('Your hand:'),
            Wrap(
              spacing: 8, runSpacing: 8,
              children: hand.map<Widget>((c) {
                final suit = (c['Suit'] ?? c['suit'] ?? '?').toString();
                final rank = (c['Rank'] ?? c['rank'] ?? '?').toString();
                return Chip(label: Text('$rank-$suit'));
              }).toList(),
            ),
            const Divider(),
            if (phase == 'pick_trump')
              Row(children: [
                const Text('Pick trump:'),
                const SizedBox(width: 8),
                Wrap(spacing: 6, children: [
                  FilledButton(onPressed: (seat == bestBy) ? () => _pickTrump('hearts') : null, child: const Text('Hearts')),
                  FilledButton(onPressed: (seat == bestBy) ? () => _pickTrump('spades') : null, child: const Text('Spades')),
                  FilledButton(onPressed: (seat == bestBy) ? () => _pickTrump('clubs') : null, child: const Text('Clubs')),
                  FilledButton(onPressed: (seat == bestBy) ? () => _pickTrump('diamonds') : null, child: const Text('Diamonds')),
                ]),
              ]),
            const Divider(),
          ],

          // EXCHANGE
          if (phase == 'exchange') ...[
            Row(children: [
              Expanded(child: Text(youAreActor ? 'Your exchange' : 'Waiting for s${actor ?? "-"}')),
              Padding(padding: const EdgeInsets.symmetric(horizontal: 6), child: Text('Talon: $talon')),
              Padding(padding: const EdgeInsets.symmetric(horizontal: 6), child: Text('Swamp: $swamp')),
            ]),
            const SizedBox(height: 8),
            if (youAreActor) Wrap(spacing: 8, children: [
              if (seat != bestBy && trump != 'clubs')
                OutlinedButton.icon(onPressed: _stayHome, icon: const Icon(Icons.door_front_door), label: const Text('Stay home')),
              FilledButton.icon(
                onPressed: _sel.isNotEmpty && _sel.length <= exchangeMax ? _exchangeSelected : null,
                icon: const Icon(Icons.swap_horiz),
                label: Text('Exchange selected (${_sel.length}/$exchangeMax)'),
              ),
              OutlinedButton(
                onPressed: _sel.isEmpty ? _exchangeDoneNoSwap : null,
                child: const Text("Done (no exchange)"),
              ),
            ]),
            const SizedBox(height: 8),
            const Text('Select cards to exchange:'),
            Wrap(
              spacing: 8, runSpacing: 8,
              children: List.generate(hand.length, (i) {
                final c = hand[i] as Map;
                final suit = (c['Suit'] ?? c['suit'] ?? '?').toString();
                final rank = (c['Rank'] ?? c['rank'] ?? '?').toString();
                final selected = _sel.contains(i);
                return FilterChip(
                  selected: selected,
                  onSelected: youAreActor ? (_) => _toggleSel(i) : null,
                  label: Text('$rank-$suit'),
                );
              }),
            ),
            const Divider(),
          ],

          // PLAY
          if (phase == 'play') ...[
            const Text('On table (current trick):'),
            Wrap(
              spacing: 8, runSpacing: 8,
              children: trick.map<Widget>((t) {
                final rank = (t['rank'] ?? t['Rank'] ?? '?').toString();
                final suit = (t['suit'] ?? t['Suit'] ?? '?').toString();
                final by = (t['by'] as num?)?.toInt();
                return Chip(label: Text('$rank-$suit  (s${by ?? "?"})'));
              }).toList(),
            ),
            const Divider(),
            const Text('Your hand:'),
            Wrap(
              spacing: 8, runSpacing: 8,
              children: hand.map<Widget>((c) {
                final suit = (c['Suit'] ?? c['suit'] ?? '?').toString();
                final rank = (c['Rank'] ?? c['rank'] ?? '?').toString();
                final stayedMe = seat != null && _stayedSeat(seat!);
                return OutlinedButton(
                  onPressed: (myPlayTurn && !handOver && !stayedMe) ? () {
                    widget.ws.send({"t":"move","m":{"room": widget.roomId, "seat": seat, "type":"play_card", "card":{"Suit": suit, "Rank": rank}}});
                  } : null,
                  child: Text('$rank-$suit'),
                );
              }).toList(),
            ),
            const Divider(),
          ],

          // Chat
          const Text('Chat:'),
          Row(children: [
            Expanded(child: TextField(controller: chatCtrl, decoration: const InputDecoration(isDense: true, hintText: 'Say something'))),
            const SizedBox(width: 8),
            FilledButton(onPressed: _sendChat, child: const Text('Send')),
          ]),
          const SizedBox(height: 10),
          Expanded(
            child: Container(
              padding: const EdgeInsets.all(8),
              decoration: BoxDecoration(border: Border.all(color: Colors.black12), borderRadius: BorderRadius.circular(6)),
              child: ListView.builder(itemCount: chat.length, itemBuilder: (_, i) => Text(chat[i])),
            ),
          ),
        ]),
      ),
    );
  }
}

// -------------------- Player ring --------------------

class PlayerRing extends StatelessWidget {
  final int seats;
  final List<String> names;
  final List<int> counts;
  final int? youSeat;
  final int? turnSeat;
  final List<int> stayed;
  const PlayerRing({super.key, required this.seats, required this.names, required this.counts, this.youSeat, this.turnSeat, required this.stayed});

  bool _stayed(int s) => stayed.contains(s);

  @override
  Widget build(BuildContext context) {
    return LayoutBuilder(builder: (context, c) {
      final w = c.maxWidth, h = c.maxHeight;
      final cx = w/2, cy = h/2;
      final r = math.min(w, h) * 0.40;
      final children = <Widget>[];

      for (int s = 0; s < seats; s++) {
        final ang = (math.pi * 1.5) + (math.pi * 2 * (s / seats));
        final x = cx + r * math.cos(ang);
        final y = cy + r * math.sin(ang);

        final name = (s < names.length && names[s].isNotEmpty) ? names[s] : 'Seat $s';
        final cnt = (s < counts.length) ? counts[s] : 0;
        final isYou = (youSeat != null && youSeat == s);
        final isTurn = (turnSeat == s);
        final isHome = _stayed(s);

        children.add(Positioned(
          left: x - 72, top: y - 34, width: 144,
          child: AnimatedContainer(
            duration: const Duration(milliseconds: 200),
            padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
            decoration: BoxDecoration(
              color: isHome ? Colors.grey.shade200 : (isTurn ? Colors.yellow.withOpacity(0.25) : Colors.white),
              borderRadius: BorderRadius.circular(14),
              border: Border.all(
                color: isTurn ? Colors.amber : (isYou ? Colors.blue : Colors.black12),
                width: isTurn ? 3 : (isYou ? 2 : 1),
              ),
              boxShadow: isTurn ? [const BoxShadow(blurRadius: 10, spreadRadius: 1, color: Colors.amberAccent)] : null,
            ),
            child: Column(mainAxisSize: MainAxisSize.min, children: [
              Text(name + (isYou ? '  (You)' : '') + (isHome ? '  (Home)' : ''), overflow: TextOverflow.ellipsis, style: TextStyle(fontWeight: isTurn ? FontWeight.w700 : FontWeight.w500)),
              Text('cards: $cnt', style: const TextStyle(fontSize: 12)),
            ]),
          ),
        ));
      }

      children.add(Positioned(left: cx-4, top: cy-4, child: const SizedBox(width: 8, height: 8, child: DecoratedBox(decoration: BoxDecoration(color: Colors.black26, shape: BoxShape.circle)))));
      return Stack(children: children);
    });
  }
}
