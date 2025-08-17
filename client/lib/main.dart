import 'dart:math' as math;
import 'package:flutter/material.dart';
import 'ws_service.dart';

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
  final nameCtrl = TextEditingController(text: '');

  @override
  void initState() { super.initState(); ws.connect(); }
  @override
  void dispose() { nameCtrl.dispose(); super.dispose(); }

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
        final list = (m['m']['list'] as List).cast<Map>().map((e) => e.map((k, v) => MapEntry(k.toString(), v))).toList();
        setState(() => rooms = List<Map<String, dynamic>>.from(list));
      } else if (m['t'] == 'created') {
        setState(() {
          createdRoomId = m['m']['room'] as String?;
          roomCtrl.text = createdRoomId ?? '';
        });
      }
    });
    widget.ws.send({"t":"list_rooms"});
  }

  @override
  void dispose() { roomCtrl.dispose(); super.dispose(); }

  void _applyName() {
    final n = widget.nameCtrl.text.trim();
    if (n.isNotEmpty) widget.ws.send({"t":"set_name","m":{"name": n}});
  }

  void _create() {
    _applyName();
    widget.ws.send({"t":"create_table","m":{"game":"mulatschak","seats":3}});
  }

  void _joinById(String id) {
    if (id.isEmpty) return;
    _applyName();
    widget.ws.send({"t":"join_table","m":{"room":id}});
    Navigator.of(context).push(MaterialPageRoute(builder: (_) => TablePage(ws: widget.ws, roomId: id)));
  }

  @override
  Widget build(BuildContext context) {
    return Padding(
      padding: const EdgeInsets.all(12),
      child: Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
        Row(children: [
          SizedBox(width: 260, child: TextField(controller: widget.nameCtrl, decoration: const InputDecoration(labelText: 'Your name (optional)', isDense: true), onSubmitted: (_) => _applyName())),
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
                    final seats = (r['seats'] as num).toInt();
                    final occ = (r['occupied'] as num).toInt();
                    final started = (r['started'] ?? false) as bool;
                    final missing = seats - occ;
                    final full = occ >= seats;
                    return ListTile(
                      title: Text('Room $id'),
                      subtitle: Text(started ? 'In play • $occ/$seats seated' : (full ? 'Full • $occ/$seats' : 'Waiting • $occ/$seats • ${missing} missing')),
                      trailing: FilledButton.tonalIcon(onPressed: full ? null : () => _joinById(id), icon: const Icon(Icons.play_arrow), label: Text(full ? 'Full' : 'Join')),
                    );
                  },
                ),
        ),
      ]),
    );
  }
}

// -------------------- Table page --------------------

class TablePage extends StatefulWidget {
  final WsService ws;
  final String roomId;
  const TablePage({super.key, required this.ws, required this.roomId});
  @override
  State<TablePage> createState() => _TablePageState();
}

class _TablePageState extends State<TablePage> {
  int? seat, turn, actor, dealer, firstBidder, bestBid, bestBy;
  String? trump, lead, phase;
  bool handOver = false, roundDouble = false;
  List<dynamic> hand = [];
  List<Map<String, dynamic>> trick = [];
  List<String> names = [];
  List<int> counts = [];
  List<int> passed = [];
  Map<String, dynamic>? cutPeek; // suit/rank, visible to cutter only

  final chat = <String>[];
  final chatCtrl = TextEditingController();

  @override
  void initState() {
    super.initState();
    widget.ws.messages.listen((m) {
      if (!mounted) return;
      switch (m['t']) {
        case 'state':
          if ((m['m']['room'] ?? '') == widget.roomId) {
            setState(() {
              seat = (m['m']['seat'] as num).toInt();
              phase = m['m']['phase'] as String?;
              actor = (m['m']['actor'] as num?)?.toInt();
              dealer = (m['m']['dealer'] as num?)?.toInt();
              firstBidder = (m['m']['firstBidder'] as num?)?.toInt();
              bestBid = (m['m']['bestBid'] as num?)?.toInt();
              bestBy = (m['m']['bestBy'] as num?)?.toInt();
              passed = ((m['m']['passed'] as List?) ?? const []).map((e) => (e as num).toInt()).toList();
              roundDouble = (m['m']['roundDouble'] ?? false) as bool;

              // cut peek (may be null unless you're cutter during bidding)
              final cp = m['m']['cutPeek'];
              cutPeek = (cp is Map) ? Map<String, dynamic>.from(cp.map((k, v) => MapEntry(k.toString(), v))) : null;

              turn = (m['m']['turn'] as num).toInt();
              trump = m['m']['trump'] as String?;
              lead  = m['m']['lead'] as String?;
              handOver = (m['m']['handOver'] ?? false) as bool;
              hand = List<dynamic>.from(m['m']['you'] as List? ?? const []);
              trick = ((m['m']['trick'] as List?) ?? const []).map((e) => Map<String, dynamic>.from((e as Map).map((k, v) => MapEntry(k.toString(), v)))).toList();
              names = ((m['m']['names'] as List?) ?? const []).map((e) => (e ?? '').toString()).toList();
              counts = ((m['m']['counts'] as List?) ?? const []).map((e) => (e as num).toInt()).toList();
            });
          }
          break;
        case 'chat':
          if ((m['m']['room'] ?? '') == widget.roomId) {
            final fromName = (m['m']['from_name'] ?? '').toString();
            final from = fromName.isNotEmpty ? fromName : (m['m']['from'] ?? 'player').toString();
            final text = (m['m']['text'] ?? '').toString();
            setState(() => chat.add('[$from] $text'));
          }
          break;
      }
    });
  }

  @override
  void dispose() { chatCtrl.dispose(); super.dispose(); }

  void _sendChat() {
    final t = chatCtrl.text.trim();
    if (t.isEmpty) return;
    widget.ws.send({"t":"chat","m":{"room": widget.roomId, "text": t}});
    chatCtrl.clear();
  }

  void _leave() {
    widget.ws.send({"t":"leave_table","m":{"room": widget.roomId}});
    Navigator.of(context).pop();
  }

  void _newHand() => widget.ws.send({"t":"new_hand","m":{"room": widget.roomId}});

  bool get myTurnToBid => phase == 'bidding' && seat != null && actor == seat;
  bool get canKnock => phase == 'bidding' && seat != null && seat == firstBidder && !(roundDouble);

  void _pass() => widget.ws.send({"t":"pass","m":{"room": widget.roomId, "seat": seat}});
  void _bid(int n) => widget.ws.send({"t":"bid","m":{"room": widget.roomId, "seat": seat, "bid": n}});
  void _knock() => widget.ws.send({"t":"knock","m":{"room": widget.roomId, "seat": seat}});
  void _pickTrump(String t) => widget.ws.send({"t":"pick_trump","m":{"room": widget.roomId, "seat": seat, "trump": t}});

  @override
  Widget build(BuildContext context) {
    final myPlayTurn = phase == 'play' && seat != null && turn != null && seat == turn;
    final occ = counts.where((c) => c > 0).length;
    final seats = counts.isNotEmpty ? counts.length : math.max(names.length, 0);

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

          SizedBox(height: 280, child: PlayerRing(seats: seats > 0 ? seats : 3, names: names, counts: counts, youSeat: seat, turnSeat: turn)),

          const Divider(),

          // ------- Cut preview for cutter during bidding -------
          if (phase == 'bidding' && seat == firstBidder && cutPeek != null) ...[
            Card(
              elevation: 0,
              color: Colors.green.withOpacity(0.12),
              child: Padding(
                padding: const EdgeInsets.all(8.0),
                child: Row(children: [
                  const Icon(Icons.content_cut),
                  const SizedBox(width: 8),
                  Text('Your cut — bottom card: ${cutPeek!['rank']}-${cutPeek!['suit']}'),
                  const Spacer(),
                  const Text('Only you can see this', style: TextStyle(fontSize: 12, fontStyle: FontStyle.italic)),
                ]),
              ),
            ),
            const SizedBox(height: 8),
          ],

          // ------- Bidding UI -------
          if (phase == 'bidding' || phase == 'pick_trump') ...[
            if (phase == 'bidding')
              Card(
                elevation: 0,
                color: Colors.yellow.withOpacity(0.12),
                child: Padding(
                  padding: const EdgeInsets.all(8.0),
                  child: Row(children: [
                    Expanded(child: Text(myTurnToBid ? 'Your turn to bid' : 'Waiting for s$actor to bid')),
                    if (canKnock) Padding(
                      padding: const EdgeInsets.only(right: 8.0),
                      child: OutlinedButton.icon(onPressed: _knock, icon: const Icon(Icons.back_hand), label: const Text('Knock (double)')),
                    ),
                    OutlinedButton(onPressed: myTurnToBid ? _pass : null, child: const Text('Pass')),
                    const SizedBox(width: 8),
                    Wrap(spacing: 6, children: [
                      FilledButton(onPressed: myTurnToBid ? () => _bid(1) : null, child: const Text('1 (Hearts)')),
                      FilledButton.tonal(onPressed: myTurnToBid ? () => _bid(2) : null, child: const Text('2')),
                      FilledButton.tonal(onPressed: myTurnToBid ? () => _bid(3) : null, child: const Text('3')),
                      FilledButton.tonal(onPressed: myTurnToBid ? () => _bid(4) : null, child: const Text('4')),
                      FilledButton.tonal(onPressed: myTurnToBid ? () => _bid(5) : null, child: const Text('5')),
                    ]),
                  ]),
                ),
              ),
            Padding(
              padding: const EdgeInsets.symmetric(vertical: 6),
              child: Text('Best bid: ${bestBid ?? 0}  by seat ${bestBy ?? "-"}  •  Passed: ${passed.map((e)=>"s$e").join(", ")}'),
            ),
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

          // ------- Play UI -------
          if (phase == 'play') ...[
            const Text('On table (current trick):'),
            Wrap(
              spacing: 8, runSpacing: 8,
              children: trick.map<Widget>((t) {
                final rank = t['rank']?.toString() ?? '?';
                final suit = t['suit']?.toString() ?? '?';
                final by = (t['by'] as num?)?.toInt();
                return Chip(label: Text('$rank-$suit  (s${by ?? "?"})'));
              }).toList(),
            ),
            const Divider(),
            const Text('Your hand:'),
            Wrap(
              spacing: 8, runSpacing: 8,
              children: hand.map<Widget>((c) {
                final suit = c['Suit'] as String? ?? '?';
                final rank = c['Rank'] as String? ?? '?';
                return OutlinedButton(
                  onPressed: (myPlayTurn && !handOver) ? () {
                    widget.ws.send({"t":"move","m":{"room": widget.roomId, "seat": seat, "type":"play_card", "card":{"Suit": suit, "Rank": rank}}});
                  } : null,
                  child: Text('$rank-$suit'),
                );
              }).toList(),
            ),
            const Divider(),
          ],

          const Text('Chat:'),
          Row(children: [
            Expanded(child: TextField(controller: chatCtrl, decoration: const InputDecoration(hintText: 'Type a message…', isDense: true), onSubmitted: (_) => _sendChat())),
            const SizedBox(width: 8),
            FilledButton(onPressed: _sendChat, child: const Text('Send')),
          ]),
          const SizedBox(height: 8),
          Expanded(
            child: Container(
              decoration: BoxDecoration(border: Border.all(color: Colors.black12), borderRadius: BorderRadius.circular(6)),
              padding: const EdgeInsets.all(8),
              child: ListView.builder(itemCount: chat.length, itemBuilder: (_, i) => Text(chat[i])),
            ),
          ),
        ]),
      ),
    );
  }
}

// -------------------- PlayerRing widget --------------------

class PlayerRing extends StatelessWidget {
  final int seats;
  final List<String> names;
  final List<int> counts;
  final int? youSeat;
  final int? turnSeat;

  const PlayerRing({super.key, required this.seats, required this.names, required this.counts, this.youSeat, this.turnSeat});

  @override
  Widget build(BuildContext context) {
    return LayoutBuilder(builder: (ctx, c) {
      final w = c.maxWidth, h = c.maxHeight;
      final r = math.min(w, h) / 2 - 36;
      final cx = w/2, cy = h/2;
      final step = 2 * math.pi / (seats == 0 ? 3 : seats);
      const start = -math.pi/2;

      List<Widget> children = [];
      for (int s = 0; s < (seats == 0 ? 3 : seats); s++) {
        final a = start + s * step;
        final x = cx + r * math.cos(a);
        final y = cy + r * math.sin(a);
        final name = (s < names.length && names[s].isNotEmpty) ? names[s] : 'Seat $s';
        final cnt  = (s < counts.length) ? counts[s] : 0;
        final isYou = (youSeat == s);
        final isTurn = (turnSeat == s);

        children.add(Positioned(
          left: x - 64, top: y - 30, width: 128,
          child: AnimatedContainer(
            duration: const Duration(milliseconds: 200),
            padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 6),
            decoration: BoxDecoration(
              color: isTurn ? Colors.yellow.withOpacity(0.25) : Colors.white,
              borderRadius: BorderRadius.circular(14),
              border: Border.all(color: isTurn ? Colors.amber : (isYou ? Colors.blue : Colors.black12), width: isTurn ? 3 : (isYou ? 2 : 1)),
              boxShadow: isTurn ? [const BoxShadow(blurRadius: 10, spreadRadius: 1, color: Colors.amberAccent)] : null,
            ),
            child: Column(mainAxisSize: MainAxisSize.min, children: [
              Text(name + (isYou ? '  (You)' : ''), overflow: TextOverflow.ellipsis, style: TextStyle(fontWeight: isTurn ? FontWeight.w700 : FontWeight.w500)),
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
