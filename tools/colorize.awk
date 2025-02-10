#!/usr/bin/awk -f


/ DEBUG /   { gsub(/ DEBUG /, "\033[1;96m&\033[0m"); }
/ ERROR /   { gsub(/ ERROR /, "\033[1;91m&\033[0m"); }
/code=500/ { gsub(/code=500/, "\033[1;91m&\033[0m"); }
/user_name=\w+? / { gsub(/user_name=\w+ /, "\033[1;92m&\033[0m"); }

/request_id=/   { gsub(/request_id=/, "\033[1;35m&\033[0m"); }

# /listen /   { gsub(/listen /, "\033[1;31m&\033[0m"); }


{ print; }
